package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
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

// Previously Claude limits came only from a running session's statusline, so
// with no Claude session they were never fetched and aged into "stale".
func TestClaudeLimitsRefreshWithoutSession(t *testing.T) {
	d := isolatedAccounts(t)
	readyClaude(t)
	var token string
	var fail error
	d.claudeUsage = func(_ context.Context, tok string) ([]hp.AccountLimitWindow, error) {
		token = tok
		return []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 30, WindowMinutes: 300}, {Name: "seven_day", UsedPercent: 60, WindowMinutes: 10080}}, fail
	}
	d.refreshAccountLimits(t.Context())
	got := claudeLimits(t)
	if token != "sk-ant-oat01-fixture" || got.State != "known" || got.Source != "claude-usage" || len(got.Windows) != 2 || got.ProfileID != "claude:central" || got.ObservedAt == 0 {
		t.Fatalf("claude refresh: %+v", got)
	}
	// The status path must neither discard nor overwrite the refreshed value.
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	for _, p := range out.Snapshot.Profiles {
		if p.ID == "claude:central" && (p.Limits.Source != "claude-usage" || len(p.Limits.Windows) != 2) {
			t.Fatalf("status dropped claude limits: %+v", p.Limits)
		}
	}
	fail = errors.New("PRIVATE vendor text")
	d.refreshAccountLimits(t.Context())
	after := claudeLimits(t)
	if len(after.Windows) != 2 || after.ObservedAt != got.ObservedAt || after.RefreshError != "claude_usage_unavailable" {
		t.Fatalf("failed claude refresh: %+v", after)
	}
	// API mode and logged-out profiles are never polled.
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
	if err != nil || len(got) != 2 || got[0].Name != "five_hour" || got[0].UsedPercent != 6 || got[0].ResetsAt != 1762232399 || got[1].Name != "seven_day" || got[1].WindowMinutes != 10080 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, bad := range []string{`not json`, `{"five_hour":{"utilization":101}}`, `{"five_hour":{"utilization":1,"resets_at":"soon"}}`} {
		if _, err := decodeClaudeUsage([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
