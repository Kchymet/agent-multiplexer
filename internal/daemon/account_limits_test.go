package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/claudecfg"
	"amux/internal/codexauth"
	"context"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"testing"
	"time"
)

// limitsProvider is a central Codex provider whose native reads can fail on
// demand, reproducing a transient app-server outage between status reads.
type limitsProvider struct {
	blockingAccountProvider
	used                       float64
	statusErr, tokenErr, limit error
	limitsStarted, release     chan struct{}
}

func (p *limitsProvider) Status(context.Context) (codexauth.Account, error) {
	if p.statusErr != nil {
		return codexauth.Account{}, p.statusErr
	}
	return codexauth.Account{Type: "chatgpt", Email: "fixture@example.test"}, nil
}
func (p *limitsProvider) SessionToken(context.Context, string, bool, string) (codexauth.Tokens, error) {
	if p.tokenErr != nil {
		return codexauth.Tokens{}, p.tokenErr
	}
	return codexauth.Tokens{AccountID: "account-a"}, nil
}
func (p *limitsProvider) RateLimits(context.Context) (hp.AccountLimits, error) {
	if p.limitsStarted != nil {
		close(p.limitsStarted)
		<-p.release
	}
	if p.limit != nil {
		return hp.AccountLimits{State: "unavailable", Source: "codex-native"}, p.limit
	}
	return hp.AccountLimits{State: "known", Source: "codex-native", ObservedAt: time.Now().UnixMilli(),
		Windows: []hp.AccountLimitWindow{{Name: "codex:primary", UsedPercent: p.used, WindowMinutes: 300}}}, nil
}

func codexStatus(t *testing.T, d *Daemon) hp.AccountProfile {
	t.Helper()
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if !out.OK || out.Snapshot == nil {
		t.Fatalf("status: %+v", out)
	}
	for _, p := range out.Snapshot.Profiles {
		if p.ID == "codex:central" {
			return p
		}
	}
	t.Fatal("codex profile missing")
	return hp.AccountProfile{}
}

// Previously a failed native read replaced the windows with "unavailable", and
// a failed identity probe made the remaining windows unattributable, so the
// rail flipped to "budget unknown" until the next good read.
func TestCodexLimitsFailureKeepsLastKnownBudget(t *testing.T) {
	d := isolatedAccounts(t)
	p := &limitsProvider{used: 40}
	d.centralCodex = &centralCodex{provider: p}
	good := codexStatus(t, d)
	if good.Limits.State != "known" || len(good.Limits.Windows) != 1 || good.Identity.State != "known" || good.Limits.RefreshError != "" {
		t.Fatalf("first read: %+v", good)
	}
	for _, tc := range []struct {
		name string
		fail func()
	}{
		{"limits", func() { p.limit = errors.New("PRIVATE native text") }},
		{"identity", func() { p.tokenErr = errors.New("timeout") }},
		{"status", func() { p.tokenErr, p.statusErr = nil, errors.New("down") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fail()
			got := codexStatus(t, d)
			if got.Limits.State != "known" || len(got.Limits.Windows) != 1 || got.Limits.Windows[0].UsedPercent != 40 {
				t.Fatalf("last-known budget dropped: %+v", got.Limits)
			}
			if got.Limits.ObservedAt != good.Limits.ObservedAt || got.Limits.AttemptedAt < good.Limits.AttemptedAt {
				t.Fatalf("failed refresh changed observation age: %+v", got.Limits)
			}
			if got.Limits.RefreshError != "native_limits_unavailable" {
				t.Fatalf("refresh failure not recorded as a fixed code: %q", got.Limits.RefreshError)
			}
			if got.Identity.State != "known" || got.Identity.AccountID != "account-a" {
				t.Fatalf("transient failure dropped verified identity: %+v", got.Identity)
			}
		})
	}
	p.statusErr, p.tokenErr, p.limit, p.used = nil, nil, nil, 55
	if got := codexStatus(t, d); got.Limits.RefreshError != "" || got.Limits.Windows[0].UsedPercent != 55 {
		t.Fatalf("recovered refresh: %+v", got.Limits)
	}
}

func readyClaude(t *testing.T) {
	t.Helper()
	secret, err := centralauth.NewSecret("sk-ant-oat01-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Update(func(s *accounts.State) error {
		cred, err := centralauth.ClaudeStore().SaveKind(secret, centralauth.KindClaudeOAuthToken, time.Now())
		if err != nil {
			return err
		}
		p := s.Profiles["claude:central"]
		p.State, p.Mode = "ready", "subscription"
		s.Profiles[p.ID] = p
		s.CredentialVersion[p.ID] = cred.Generation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func claudeLimits(t *testing.T) hp.AccountLimits {
	t.Helper()
	s, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	return s.Profiles["claude:central"].Limits
}

// accountClock is a settable daemon account clock.
func accountClock(d *Daemon) *time.Time {
	now := time.Now()
	d.accountNow = func() time.Time { return now }
	return &now
}

// Previously Claude limits came only from a running session's statusline, so
// with no Claude session they were never fetched and aged into "stale".
func TestClaudeLimitsRefreshWithoutSession(t *testing.T) {
	d := isolatedAccounts(t)
	now := accountClock(d)
	readyClaude(t)
	var token string
	var fail error
	two := int64(2)
	d.claudeUsage = func(_ context.Context, tok string) (claudeUsageReport, error) {
		token = tok
		return claudeUsageReport{Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 30, WindowMinutes: 300}, {Name: "seven_day", UsedPercent: 60, WindowMinutes: 10080}}, Resets: &two}, fail
	}
	d.refreshAccountLimits(t.Context())
	got := claudeLimits(t)
	if token != "sk-ant-oat01-fixture" || got.State != "known" || got.Source != "claude-usage" || len(got.Windows) != 2 || got.ProfileID != "claude:central" || got.ObservedAt == 0 {
		t.Fatalf("claude refresh: %+v", got)
	}
	if got.ResetsAvailable == nil || *got.ResetsAvailable != 2 {
		t.Fatalf("reset count not recorded: %+v", got)
	}
	// The status path must neither discard nor overwrite the refreshed value.
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	for _, p := range out.Snapshot.Profiles {
		if p.ID == "claude:central" && (p.Limits.Source != "claude-usage" || len(p.Limits.Windows) != 2) {
			t.Fatalf("status dropped claude limits: %+v", p.Limits)
		}
	}
	*now = now.Add(claudeUsageInterval)
	fail = errors.New("PRIVATE vendor text")
	d.refreshAccountLimits(t.Context())
	after := claudeLimits(t)
	if len(after.Windows) != 2 || after.ObservedAt != got.ObservedAt || after.RefreshError != "claude_usage_unavailable" || after.RetryAt != 0 {
		t.Fatalf("failed claude refresh: %+v", after)
	}
	// API mode and logged-out profiles are never polled.
	*now = now.Add(claudeUsageInterval)
	token = ""
	_ = accounts.Update(func(s *accounts.State) error {
		p := s.Profiles["claude:central"]
		p.State = "logged-out"
		s.Profiles[p.ID] = p
		return nil
	})
	d.refreshAccountLimits(t.Context())
	if token != "" {
		t.Fatal("logged-out profile was polled")
	}
}

// The usage endpoint locks a credential out for up to an hour once it is read
// too often. Previously every 2-minute tick read it again, ignoring
// Retry-After, and the budget went stale with no explanation.
func TestClaudeUsageRateLimitBacksOff(t *testing.T) {
	d := isolatedAccounts(t)
	now := accountClock(d)
	readyClaude(t)
	calls := 0
	var fail error
	d.claudeUsage = func(context.Context, string) (claudeUsageReport, error) {
		calls++
		return claudeUsageReport{Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 30, WindowMinutes: 300}}}, fail
	}
	d.refreshAccountLimits(t.Context())
	good := claudeLimits(t)
	// Ticks inside the polling interval do not read again.
	*now = now.Add(accountLimitsRefreshInterval)
	d.refreshAccountLimits(t.Context())
	if calls != 1 {
		t.Fatalf("read %d times inside the polling interval", calls)
	}
	*now = now.Add(accountLimitsRefreshInterval)
	fail = claudeUsageStatus(429, "1800", nil)
	d.refreshAccountLimits(t.Context())
	limited := claudeLimits(t)
	if calls != 2 || limited.RefreshError != "claude_usage_rate_limited" || limited.RetryAt != now.Add(30*time.Minute).UnixMilli() {
		t.Fatalf("rate limit not recorded with its retry time: %+v", limited)
	}
	if len(limited.Windows) != 1 || limited.ObservedAt != good.ObservedAt {
		t.Fatalf("rate limit dropped the last-known budget: %+v", limited)
	}
	// Neither ticks during the pause nor a restarted daemon read again.
	restarted := isolatedRestart(t, d)
	for range 3 {
		*now = now.Add(5 * time.Minute)
		d.refreshAccountLimits(t.Context())
		restarted.refreshAccountLimits(t.Context())
	}
	if calls != 2 || claudeLimits(t).RetryAt != limited.RetryAt {
		t.Fatalf("read during the vendor's pause: %d calls, %+v", calls, claudeLimits(t))
	}
	*now = now.Add(16 * time.Minute)
	fail = nil
	d.refreshAccountLimits(t.Context())
	if got := claudeLimits(t); calls != 3 || got.RefreshError != "" || got.RetryAt != 0 {
		t.Fatalf("no recovery after the pause: %d calls, %+v", calls, got)
	}
}

// isolatedRestart is a second daemon over the same account store, without
// the first one's in-memory state.
func isolatedRestart(t *testing.T, d *Daemon) *Daemon {
	r := &Daemon{accountNow: d.accountNow, claudeUsage: d.claudeUsage}
	t.Cleanup(func() { r.disconnectAccounts("", true) })
	return r
}

func TestClaudeUsageStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		retry  string
		body   string
		code   string
		pause  time.Duration
	}{
		{429, "", "", "claude_usage_rate_limited", 5 * time.Minute},
		{429, "120", "", "claude_usage_rate_limited", 2 * time.Minute},
		{429, "99999", "", "claude_usage_rate_limited", time.Hour},
		{403, "", `{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement any_of(user:profile, user:office)","details":{"error_code":"oauth_scope_insufficient"}}}`, "claude_usage_scope", time.Hour},
		{401, "", "", "claude_usage_unauthorized", time.Hour},
		{500, "", "", "claude_usage_unavailable", 0},
	} {
		var ue *claudeUsageError
		if !errors.As(claudeUsageStatus(tc.status, tc.retry, []byte(tc.body)), &ue) || ue.code != tc.code || ue.pause != tc.pause {
			t.Fatalf("%d %q: %+v", tc.status, tc.retry, ue)
		}
	}
}

// Previously only central profiles were refreshed, so a host or shared login
// selected for new workloads never had a budget.
func TestHostLoginsRefresh(t *testing.T) {
	d := isolatedAccounts(t)
	now := accountClock(d)
	if err := accounts.Update(func(s *accounts.State) error {
		s.Defaults["claude"], s.Defaults["codex"] = "claude:host", "codex:host"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	login := claudecfg.OAuthLogin{AccessToken: "sk-ant-oat01-host", ExpiresAt: now.Add(time.Hour), Scopes: []string{"user:inference", "user:profile"}}
	d.claudeLogin = func(mode string) (claudecfg.OAuthLogin, error) {
		if mode != "host" {
			t.Fatalf("read %s login", mode)
		}
		return login, nil
	}
	var token string
	d.claudeUsage = func(_ context.Context, tok string) (claudeUsageReport, error) {
		token = tok
		return claudeUsageReport{Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 30, WindowMinutes: 300}}}, nil
	}
	one := int64(1)
	d.hostCodexLimits = func(context.Context) (hp.AccountLimits, error) {
		return hp.AccountLimits{State: "known", Source: "codex-native", Windows: []hp.AccountLimitWindow{{Name: "codex:primary", UsedPercent: 20, WindowMinutes: 300}}, ResetsAvailable: &one}, nil
	}
	d.refreshAccountLimits(t.Context())
	s, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	if c := s.Profiles["claude:host"].Limits; token != "sk-ant-oat01-host" || c.State != "known" || c.ProfileID != "claude:host" {
		t.Fatalf("host claude: %q %+v", token, c)
	}
	if c := s.Profiles["codex:host"].Limits; c.State != "known" || c.ResetsAvailable == nil || *c.ResetsAvailable != 1 || c.ProfileID != "codex:host" {
		t.Fatalf("host codex: %+v", c)
	}
	if s.Profiles["claude:shared"].Limits.State == "known" || s.Profiles["claude:environment"].Limits.State == "known" {
		t.Fatal("polled a profile no workload uses")
	}
	// An expired access token or a login without the profile scope is
	// reported, never refreshed or sent.
	for _, tc := range []struct {
		login claudecfg.OAuthLogin
		code  string
	}{
		{claudecfg.OAuthLogin{AccessToken: "x", ExpiresAt: now.Add(-time.Minute)}, "credential_expired"},
		{claudecfg.OAuthLogin{AccessToken: "x", Scopes: []string{"user:inference"}}, "claude_usage_scope"},
	} {
		*now = now.Add(claudeUsageInterval)
		login, token = tc.login, ""
		d.refreshAccountLimits(t.Context())
		s, _ = accounts.Read()
		if c := s.Profiles["claude:host"].Limits; token != "" || c.RefreshError != tc.code || len(c.Windows) != 1 {
			t.Fatalf("%s: sent %q, %+v", tc.code, token, c)
		}
	}
}

// A status read that started before a newer observation must not publish its
// older copy of the limits over it.
func TestAccountStatusKeepsConcurrentObservation(t *testing.T) {
	d := isolatedAccounts(t)
	p := &limitsProvider{used: 10, limitsStarted: make(chan struct{}), release: make(chan struct{})}
	d.centralCodex = &centralCodex{provider: p}
	done := make(chan hp.AccountResult, 1)
	go func() {
		done <- d.accountCall(context.Background(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	}()
	<-p.limitsStarted
	fresh := hp.AccountLimits{State: "known", Source: "claude-statusline", ProfileID: "claude:central", DeliveryEpoch: 1, ObservedAt: time.Now().UnixMilli(),
		Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 12}}}
	if err := accounts.Update(func(s *accounts.State) error {
		q := s.Profiles["claude:central"]
		q.Limits = fresh
		s.Profiles[q.ID] = q
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	<-done
	if got := claudeLimits(t); got.ObservedAt != fresh.ObservedAt || len(got.Windows) != 1 {
		t.Fatalf("concurrent observation overwritten: %+v", got)
	}
}

func TestDecodeClaudeUsage(t *testing.T) {
	got, err := decodeClaudeUsage([]byte(`{"five_hour":{"utilization":6.0,"resets_at":"2025-11-04T04:59:59.943648+00:00"},"seven_day":{"utilization":35,"resets_at":null},"seven_day_opus":{"utilization":1}}`))
	w := got.Windows
	if err != nil || len(w) != 2 || w[0].Name != "five_hour" || w[0].UsedPercent != 6 || w[0].ResetsAt != 1762232399 || w[1].Name != "seven_day" || w[1].WindowMinutes != 10080 || got.Resets != nil {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{`not json`, `{"five_hour":{"utilization":101}}`, `{"five_hour":{"utilization":1,"resets_at":"soon"}}`, `{"cedar_ember":{"eligible":true,"grants":[{"resets_left":-1}]}}`} {
		if _, err := decodeClaudeUsage([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	// Reset grants count what is left on each grant that is not paused.
	got, err = decodeClaudeUsage([]byte(`{"five_hour":{"utilization":6},"cedar_ember":{"eligible":true,"grants":[{"id":"a","resets_total":3,"resets_left":2,"paused":false},{"id":"b","resets_left":1,"paused":true},{"id":"c","resets_left":1}]}}`))
	if err != nil || got.Resets == nil || *got.Resets != 3 {
		t.Fatalf("resets: %+v %v", got, err)
	}
	got, err = decodeClaudeUsage([]byte(`{"cedar_ember":{"eligible":false,"ineligible_reason":"tier"}}`))
	if err != nil || got.Resets != nil {
		t.Fatalf("an ineligible account reported a count: %+v %v", got, err)
	}
}
