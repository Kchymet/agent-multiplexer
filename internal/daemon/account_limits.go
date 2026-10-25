package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"context"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"io"
	"net/http"
	"time"
)

// accountLimitsRefreshInterval keeps every configured subscription budget
// fresh without an attached client or running session. It is well inside
// AccountLimitsTTLMillis, so one failed refresh does not make a value stale.
const accountLimitsRefreshInterval = 2 * time.Minute

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

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

// refreshAccountLimits polls every ready central subscription profile, so the
// Claude budget does not depend on a session's statusline being active.
func (d *Daemon) refreshAccountLimits(ctx context.Context) {
	s, err := accounts.Read()
	if err != nil {
		return
	}
	for _, p := range s.Profiles {
		if p.Source != "central" || p.State != "ready" || p.Mode == "api" {
			continue
		}
		var limits hp.AccountLimits
		switch {
		case p.Harness == "claude":
			limits, err = d.fetchClaudeLimits(ctx)
		case p.Harness == "codex" && d.centralCodex != nil && d.centralCodex.provider != nil:
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			limits, err = d.centralCodex.provider.RateLimits(qctx)
			cancel()
			if err != nil {
				err = errors.New("native_limits_unavailable")
			}
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

// mergeAccountLimits records one refresh. Success replaces the observation;
// failure keeps the profile's last-known windows and their ObservedAt, so the
// budget ages visibly instead of collapsing to unknown.
func mergeAccountLimits(p hp.AccountProfile, next hp.AccountLimits, err error, now time.Time) hp.AccountLimits {
	prev := p.Limits
	if err == nil {
		next.ProfileID, next.DeliveryEpoch = p.ID, p.DeliveryEpoch
		next.AttemptedAt, next.RefreshError = now.UnixMilli(), ""
		if next.ObservedAt == 0 {
			next.ObservedAt = now.UnixMilli()
		}
		return next
	}
	if prev.ProfileID != p.ID || prev.DeliveryEpoch != p.DeliveryEpoch {
		prev = hp.AccountLimits{State: "unavailable", Source: next.Source, ProfileID: p.ID, DeliveryEpoch: p.DeliveryEpoch}
	}
	prev.AttemptedAt, prev.RefreshError = now.UnixMilli(), err.Error()
	return prev
}

// newerAccountLimits reports whether a holds a later observation or refresh
// attempt than b; concurrent status reads must not overwrite it.
func newerAccountLimits(a, b hp.AccountLimits) bool {
	return a.ObservedAt > b.ObservedAt || (a.ObservedAt == b.ObservedAt && a.AttemptedAt > b.AttemptedAt)
}

func (d *Daemon) fetchClaudeLimits(ctx context.Context) (hp.AccountLimits, error) {
	cred, err := centralauth.ClaudeStore().Load()
	if err != nil || cred.Kind != centralauth.KindClaudeOAuthToken || cred.Token.Empty() {
		return hp.AccountLimits{Source: "claude-usage"}, errors.New("credential_unavailable")
	}
	fetch := d.claudeUsage
	if fetch == nil {
		fetch = fetchClaudeUsage
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	windows, err := fetch(qctx, cred.Token.Reveal())
	if err != nil {
		// Never surface transport or vendor text: it may echo request data.
		return hp.AccountLimits{Source: "claude-usage"}, errors.New("claude_usage_unavailable")
	}
	state := "unknown"
	if len(windows) > 0 {
		state = "known"
	}
	return hp.AccountLimits{State: state, Source: "claude-usage", ObservedAt: d.accountTime().UnixMilli(), Windows: windows}, nil
}

// fetchClaudeUsage reads the subscription usage report that Claude Code's
// /usage command shows for the same OAuth credential.
func fetchClaudeUsage(ctx context.Context, token string) ([]hp.AccountLimitWindow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("claude_usage_status")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	return decodeClaudeUsage(raw)
}

func decodeClaudeUsage(raw []byte) ([]hp.AccountLimitWindow, error) {
	type window struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var data struct {
		Five *window `json:"five_hour"`
		Week *window `json:"seven_day"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil, errors.New("claude_usage_invalid")
	}
	out := []hp.AccountLimitWindow{}
	for _, v := range []struct {
		name    string
		minutes int64
		w       *window
	}{{"five_hour", 300, data.Five}, {"seven_day", 10080, data.Week}} {
		if v.w == nil || v.w.Utilization == nil {
			continue
		}
		if *v.w.Utilization < 0 || *v.w.Utilization > 100 {
			return nil, errors.New("claude_usage_invalid")
		}
		w := hp.AccountLimitWindow{Name: v.name, UsedPercent: *v.w.Utilization, WindowMinutes: v.minutes}
		if v.w.ResetsAt != "" {
			reset, err := time.Parse(time.RFC3339Nano, v.w.ResetsAt)
			if err != nil {
				return nil, errors.New("claude_usage_invalid")
			}
			w.ResetsAt = reset.Unix()
		}
		out = append(out, w)
	}
	return out, nil
}
