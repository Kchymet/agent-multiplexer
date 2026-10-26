package daemon

import (
	"amux/internal/accounts"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/claudecfg"
	"amux/internal/codexauth"
	"amux/internal/codexcfg"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"
)

// accountLimitsRefreshInterval keeps every configured subscription budget
// fresh without an attached client or running session. It is well inside
// AccountLimitsTTLMillis, so one failed refresh does not make a value stale.
const accountLimitsRefreshInterval = 2 * time.Minute

// claudeUsageInterval spaces Claude usage reads further apart: the endpoint
// locks a credential out for up to an hour once it is read too often, and
// Claude sessions holding the same login read it too. It stays inside
// AccountLimitsTTLMillis; one tick of slack absorbs loop jitter.
const (
	claudeUsageInterval = 4 * time.Minute
	claudeUsageSlack    = 30 * time.Second
)

// cedar_ember asks the usage report to include the account's rate-limit
// reset grants, as Claude Code's own reset read does.
const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"

// Claude Code pauses a bearer's usage reads after a 429 or 403 for the
// response's Retry-After, 5 minutes by default, at most an hour. A refused
// credential (401, or a token without the profile scope) cannot recover until
// a new login, which changes the credential and so ends the pause early.
const (
	claudeUsageDefaultPause = 5 * time.Minute
	claudeUsageMaxPause     = time.Hour
)

// A `claude setup-token` token has only the user:inference scope, so the
// usage report refuses it. Its windows come instead from the unified
// rate-limit headers of a one-token Haiku request, the same quota probe
// Claude Code sends. Each probe spends about 8 input and 1 output token of the
// subscription, so it runs at most every claudeProbeInterval and not while a
// session's statusline or an earlier probe is newer than that.
const (
	claudeMessagesURL   = "https://api.anthropic.com/v1/messages"
	claudeProbeModel    = "claude-haiku-4-5"
	claudeProbeInterval = 15 * time.Minute
)

// Fixed refresh codes; vendor and transport text is never surfaced.
const (
	errClaudeUsageUnavailable = "claude_usage_unavailable"
	errClaudeUsageRateLimited = "claude_usage_rate_limited"
	errClaudeUsageScope       = "claude_usage_scope"
	errClaudeUsageRejected    = "claude_usage_unauthorized"
	errClaudeProbe            = "claude_probe_unavailable"
	errCredentialUnavailable  = "credential_unavailable"
	errCredentialExpired      = "credential_expired"
	errNativeLimits           = "native_limits_unavailable"
)

// claudeUsageError is a failed usage read with the pause it calls for.
type claudeUsageError struct {
	code  string
	pause time.Duration
}

func (e *claudeUsageError) Error() string { return e.code }

// claudeUsageReport is the part of the usage report amux shows.
type claudeUsageReport struct {
	Windows []hp.AccountLimitWindow
	Resets  *int64 // redeemable reset grants; nil when not reported
}

// accountLimitsLoop refreshes at startup and then periodically until ctx ends.
func (d *Daemon) accountLimitsLoop(ctx context.Context) {
	t := time.NewTicker(accountLimitsRefreshInterval)
	defer t.Stop()
	for {
		d.refreshAccountLimits(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshAccountLimits polls every ready subscription profile that new
// workloads use or that is central, so the budget does not depend on a
// session's statusline being active. Environment profiles hold no credential
// amux can read.
func (d *Daemon) refreshAccountLimits(ctx context.Context) {
	s, err := accounts.Read()
	if err != nil {
		return
	}
	for _, p := range s.Profiles {
		if p.State != "ready" || p.Mode == "api" {
			continue
		}
		if p.Source != amuxcfg.AuthCentral && s.Defaults[p.Harness] != p.ID {
			continue
		}
		var limits hp.AccountLimits
		switch {
		case p.Harness == "claude" && (p.Source == amuxcfg.AuthCentral || p.Source == amuxcfg.AuthHost || p.Source == amuxcfg.AuthShared):
			var skip bool
			if limits, skip, err = d.fetchClaudeLimits(ctx, p); skip {
				continue
			}
		case p.Harness == "codex" && p.Source == amuxcfg.AuthCentral && d.centralCodex != nil && d.centralCodex.provider != nil:
			limits, err = codexLimits(ctx, 10*time.Second, d.centralCodex.provider.RateLimits)
		case p.Harness == "codex" && p.Source == amuxcfg.AuthHost:
			// A transient native server needs time to start and initialize.
			limits, err = codexLimits(ctx, 45*time.Second, d.readHostCodexLimits)
		default:
			continue
		}
		if ctx.Err() != nil {
			return
		}
		_ = accounts.Update(func(current *accounts.State) error {
			q := current.Profiles[p.ID]
			// Login, logout, and mode changes retire this observation.
			if q.DeliveryEpoch != p.DeliveryEpoch || q.State != "ready" || q.Mode != p.Mode {
				return nil
			}
			q.Limits = mergeAccountLimits(q, limits, err, d.accountTime())
			current.Profiles[q.ID] = q
			return nil
		})
	}
}

func codexLimits(ctx context.Context, timeout time.Duration, read func(context.Context) (hp.AccountLimits, error)) (hp.AccountLimits, error) {
	qctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	limits, err := read(qctx)
	if err != nil {
		err = errors.New(errNativeLimits)
	}
	return limits, err
}

// readHostCodexLimits reads the user's own Codex home, which codex.auth host
// sessions share.
func (d *Daemon) readHostCodexLimits(ctx context.Context) (hp.AccountLimits, error) {
	if d.hostCodexLimits != nil {
		return d.hostCodexLimits(ctx)
	}
	bin, err := codexExecutable()
	if err != nil {
		return hp.AccountLimits{Source: "codex-native"}, err
	}
	return codexauth.ReadRateLimits(ctx, codexauth.Config{Argv: []string{bin}, Home: codexcfg.UserHome().Dir()})
}

// mergeAccountLimits records one refresh. Success replaces the observation;
// failure keeps the profile's last-known windows and their ObservedAt, so the
// budget ages visibly instead of collapsing to unknown.
func mergeAccountLimits(p hp.AccountProfile, next hp.AccountLimits, err error, now time.Time) hp.AccountLimits {
	prev := p.Limits
	if err == nil {
		next.ProfileID, next.DeliveryEpoch = p.ID, p.DeliveryEpoch
		next.AttemptedAt, next.RefreshError, next.RetryAt = now.UnixMilli(), "", 0
		if next.ObservedAt == 0 {
			next.ObservedAt = now.UnixMilli()
		}
		return next
	}
	if prev.ProfileID != p.ID || prev.DeliveryEpoch != p.DeliveryEpoch {
		prev = hp.AccountLimits{State: "unavailable", Source: next.Source, ProfileID: p.ID, DeliveryEpoch: p.DeliveryEpoch}
	}
	prev.AttemptedAt, prev.RefreshError, prev.RetryAt = now.UnixMilli(), err.Error(), 0
	if ue := (*claudeUsageError)(nil); errors.As(err, &ue) && ue.pause > 0 {
		prev.RetryAt = now.Add(ue.pause).UnixMilli()
	}
	return prev
}

// newerAccountLimits reports whether a holds a later observation or refresh
// attempt than b; concurrent status reads must not overwrite it.
func newerAccountLimits(a, b hp.AccountLimits) bool {
	return a.ObservedAt > b.ObservedAt || (a.ObservedAt == b.ObservedAt && a.AttemptedAt > b.AttemptedAt)
}

// claudeToken is the profile's own credential: the central setup-token, or
// the access token of the host or shared interactive login. An interactive
// login that states its scopes and lacks the profile scope is refused before
// any request.
func (d *Daemon) claudeToken(p hp.AccountProfile) (string, error) {
	if p.Source == amuxcfg.AuthCentral {
		cred, err := centralauth.ClaudeStore().Load()
		if err != nil || cred.Kind != centralauth.KindClaudeOAuthToken || cred.Token.Empty() {
			return "", errors.New(errCredentialUnavailable)
		}
		return cred.Token.Reveal(), nil
	}
	read := d.claudeLogin
	if read == nil {
		read = claudecfg.ReadOAuthLogin
	}
	login, err := read(p.Source)
	if err != nil {
		return "", errors.New(errCredentialUnavailable)
	}
	if !login.ExpiresAt.IsZero() && !d.accountTime().Before(login.ExpiresAt) {
		// Claude renews it on its next use; amux never spends the refresh token.
		return "", errors.New(errCredentialExpired)
	}
	if has, known := login.HasScope(claudecfg.ProfileScope); known && !has {
		return "", errors.New(errClaudeUsageScope)
	}
	return login.AccessToken, nil
}

// fetchClaudeLimits reads the usage report for p's credential. skip means the
// vendor asked amux to pause this credential's reads and nothing is recorded.
func (d *Daemon) fetchClaudeLimits(ctx context.Context, p hp.AccountProfile) (hp.AccountLimits, bool, error) {
	out := hp.AccountLimits{Source: "claude-usage"}
	token, err := d.claudeToken(p)
	if err != nil {
		return out, false, err
	}
	now := d.accountTime()
	key := sha256.Sum256([]byte(token))
	central := p.Source == amuxcfg.AuthCentral
	if central && d.claudeNoUsage[p.ID] == key {
		return d.probeClaudeLimits(ctx, p, token, key)
	}
	if last := p.Limits; last.ProfileID == p.ID && last.DeliveryEpoch == p.DeliveryEpoch {
		// The persisted pause outlives a daemon restart. A credential other
		// than the paused one (a renewed host login) may read at once.
		if paused, known := d.claudePaused[p.ID]; last.RetryAt > now.UnixMilli() && (!known || paused == key) {
			return out, true, nil
		}
		if last.RetryAt <= now.UnixMilli() && last.AttemptedAt > 0 && now.Sub(time.UnixMilli(last.AttemptedAt)) < claudeUsageInterval-claudeUsageSlack {
			return out, true, nil
		}
	}
	fetch := d.claudeUsage
	if fetch == nil {
		fetch = fetchClaudeUsage
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	report, err := fetch(qctx, token)
	delete(d.claudePaused, p.ID)
	if err != nil {
		var ue *claudeUsageError
		if !errors.As(err, &ue) {
			// Never surface transport or vendor text: it may echo request data.
			return out, false, errors.New(errClaudeUsageUnavailable)
		}
		if ue.code == errClaudeUsageScope && central {
			// Remembered for this credential; a new login reads the report again.
			if d.claudeNoUsage == nil {
				d.claudeNoUsage = map[string][32]byte{}
			}
			d.claudeNoUsage[p.ID] = key
			return d.probeClaudeLimits(ctx, p, token, key)
		}
		if ue.pause > 0 {
			if d.claudePaused == nil {
				d.claudePaused = map[string][32]byte{}
			}
			d.claudePaused[p.ID] = key
		}
		return out, false, &claudeUsageError{code: ue.code, pause: ue.pause}
	}
	out.State, out.ObservedAt = "unknown", now.UnixMilli()
	out.Windows, out.ResetsAvailable = report.Windows, report.Resets
	if len(report.Windows) > 0 {
		out.State = "known"
	}
	return out, false, nil
}

// fetchClaudeUsage reads the subscription usage report that Claude Code's
// /usage command shows for the same OAuth credential.
func fetchClaudeUsage(ctx context.Context, token string) (claudeUsageReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return claudeUsageReport{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return claudeUsageReport{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return claudeUsageReport{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return claudeUsageReport{}, claudeUsageStatus(resp.StatusCode, resp.Header.Get("Retry-After"), raw)
	}
	return decodeClaudeUsage(raw)
}

// claudeUsageStatus classifies a refused read into a fixed code and pause.
func claudeUsageStatus(status int, retryAfter string, body []byte) error {
	pause := claudeUsageDefaultPause
	if secs, err := strconv.Atoi(retryAfter); err == nil && secs > 0 {
		pause = min(time.Duration(secs)*time.Second, claudeUsageMaxPause)
	}
	switch status {
	case http.StatusTooManyRequests:
		return &claudeUsageError{code: errClaudeUsageRateLimited, pause: pause}
	case http.StatusUnauthorized:
		return &claudeUsageError{code: errClaudeUsageRejected, pause: claudeUsageMaxPause}
	case http.StatusForbidden:
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Details struct {
					Code string `json:"error_code"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error.Details.Code == "oauth_scope_insufficient" || e.Error.Type == "permission_error" {
			return &claudeUsageError{code: errClaudeUsageScope, pause: claudeUsageMaxPause}
		}
		return &claudeUsageError{code: errClaudeUsageUnavailable, pause: pause}
	}
	return &claudeUsageError{code: errClaudeUsageUnavailable}
}

func decodeClaudeUsage(raw []byte) (claudeUsageReport, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	type grant struct {
		Left   *int64 `json:"resets_left"`
		Paused bool   `json:"paused"`
	}
	var data struct {
		Five   *window `json:"five_hour"`
		Week   *window `json:"seven_day"`
		Resets *struct {
			Eligible bool    `json:"eligible"`
			Grants   []grant `json:"grants"`
		} `json:"cedar_ember"`
	}
	invalid := &claudeUsageError{code: "claude_usage_invalid"}
	if json.Unmarshal(raw, &data) != nil {
		return claudeUsageReport{}, invalid
	}
	out := claudeUsageReport{Windows: []hp.AccountLimitWindow{}}
	for _, v := range []struct {
		name    string
		minutes int64
		w       *window
	}{{"five_hour", 300, data.Five}, {"seven_day", 10080, data.Week}} {
		if v.w == nil || v.w.Utilization == nil {
			continue
		}
		if *v.w.Utilization < 0 || *v.w.Utilization > 100 {
			return claudeUsageReport{}, invalid
		}
		w := hp.AccountLimitWindow{Name: v.name, UsedPercent: *v.w.Utilization, WindowMinutes: v.minutes}
		if v.w.ResetsAt != "" {
			reset, err := time.Parse(time.RFC3339Nano, v.w.ResetsAt)
			if err != nil {
				return claudeUsageReport{}, invalid
			}
			w.ResetsAt = reset.Unix()
		}
		out.Windows = append(out.Windows, w)
	}
	// Claude Code counts the resets left on every grant that is not paused.
	// An ineligible account reports no count rather than zero.
	if r := data.Resets; r != nil && r.Eligible {
		var total int64
		for _, g := range r.Grants {
			if g.Left == nil || *g.Left < 0 {
				return claudeUsageReport{}, invalid
			}
			if !g.Paused {
				total += *g.Left
			}
		}
		out.Resets = &total
	}
	return out, nil
}

// probeClaudeLimits reads the central setup-token's windows from a quota
// probe. skip means a newer observation or a pause makes a probe unnecessary.
func (d *Daemon) probeClaudeLimits(ctx context.Context, p hp.AccountProfile, token string, key [32]byte) (hp.AccountLimits, bool, error) {
	out := hp.AccountLimits{Source: "claude-probe"}
	now := d.accountTime()
	last := p.Limits
	attributed := last.ProfileID == p.ID && last.DeliveryEpoch == p.DeliveryEpoch
	if attributed {
		if paused, known := d.claudePaused[p.ID]; last.RetryAt > now.UnixMilli() && (!known || paused == key) {
			return out, true, nil
		}
		recent := func(ms int64) bool {
			return ms > 0 && now.Sub(time.UnixMilli(ms)) < claudeProbeInterval-claudeUsageSlack
		}
		if last.RetryAt <= now.UnixMilli() && (recent(last.ObservedAt) || last.Source == "claude-probe" && recent(last.AttemptedAt)) {
			return out, true, nil
		}
	}
	probe := d.claudeProbe
	if probe == nil {
		probe = fetchClaudeProbe
	}
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	windows, err := probe(qctx, token)
	delete(d.claudePaused, p.ID)
	if err != nil {
		var ue *claudeUsageError
		if !errors.As(err, &ue) {
			return out, false, errors.New(errClaudeProbe)
		}
		if ue.pause > 0 {
			if d.claudePaused == nil {
				d.claudePaused = map[string][32]byte{}
			}
			d.claudePaused[p.ID] = key
		}
		return out, false, &claudeUsageError{code: ue.code, pause: ue.pause}
	}
	out.State, out.ObservedAt, out.Windows = "unknown", now.UnixMilli(), windows
	// Nothing else refreshes it sooner, so it stays current until the next probe.
	out.FreshUntil = now.Add(claudeProbeInterval + accountLimitsRefreshInterval).UnixMilli()
	if len(windows) > 0 {
		out.State = "known"
	}
	if attributed {
		out.ResetsAvailable = last.ResetsAvailable // the probe carries no reset count
	}
	return out, false, nil
}

// fetchClaudeProbe sends Claude Code's one-token quota probe and reads the
// unified rate-limit headers. An at-limit 429 still carries them.
func fetchClaudeProbe(ctx context.Context, token string) ([]hp.AccountLimitWindow, error) {
	body, err := json.Marshal(map[string]any{"model": claudeProbeModel, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "quota"}}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeMessagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
		if windows, ok := decodeClaudeRateLimitHeaders(resp.Header); ok {
			return windows, nil
		}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return nil, &claudeUsageError{code: errClaudeProbe}
	case http.StatusTooManyRequests, http.StatusUnauthorized:
		return nil, claudeUsageStatus(resp.StatusCode, resp.Header.Get("Retry-After"), nil)
	}
	return nil, &claudeUsageError{code: errClaudeProbe, pause: claudeUsageDefaultPause}
}

// decodeClaudeRateLimitHeaders reads the anthropic-ratelimit-unified-*
// windows: utilization is a 0-1 fraction, resets are Unix seconds. ok is
// false when no window header is present.
func decodeClaudeRateLimitHeaders(h http.Header) ([]hp.AccountLimitWindow, bool) {
	out := []hp.AccountLimitWindow{}
	for _, v := range []struct {
		name, key string
		minutes   int64
	}{{"five_hour", "5h", 300}, {"seven_day", "7d", 10080}} {
		util, err := strconv.ParseFloat(h.Get("anthropic-ratelimit-unified-"+v.key+"-utilization"), 64)
		if err != nil || util < 0 || util > 1 {
			continue
		}
		w := hp.AccountLimitWindow{Name: v.name, UsedPercent: math.Round(util*1000) / 10, WindowMinutes: v.minutes}
		if reset, err := strconv.ParseInt(h.Get("anthropic-ratelimit-unified-"+v.key+"-reset"), 10, 64); err == nil && reset > 0 {
			w.ResetsAt = reset
		}
		out = append(out, w)
	}
	return out, len(out) > 0
}
