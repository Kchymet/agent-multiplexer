package daemon

import (
	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/core"
	"amux/internal/store"
	"context"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"sync/atomic"
	"testing"
	"time"
)

type blockingAccountProvider struct{ started, release chan struct{} }

func (p *blockingAccountProvider) Start(context.Context) error { return nil }
func (p *blockingAccountProvider) Close() error                { return nil }
func (p *blockingAccountProvider) ForgetSession(string)        {}
func (p *blockingAccountProvider) SessionToken(context.Context, string, bool, string) (codexauth.Tokens, error) {
	close(p.started)
	<-p.release
	return codexauth.Tokens{AccessToken: "SECRET", AccountID: "account-a", Generation: 1}, nil
}
func (p *blockingAccountProvider) Login(context.Context, func(string), ...func() error) error {
	return nil
}
func (p *blockingAccountProvider) Logout(context.Context) error { return nil }
func (p *blockingAccountProvider) Status(context.Context) (codexauth.Account, error) {
	return codexauth.Account{}, nil
}
func (p *blockingAccountProvider) RateLimits(context.Context) (hp.AccountLimits, error) {
	return hp.AccountLimits{}, nil
}
func TestCodexRefreshRechecksArchiveRevokeLogoutAndBinding(t *testing.T) {
	for _, change := range []string{"archive", "revoke", "logout", "rebind"} {
		t.Run(change, func(t *testing.T) {
			row := store.Session{ID: "w", Agent: "codex", Dir: t.TempDir()}
			d, _, _ := sessionRuntimeFixture(t, row)
			p := &blockingAccountProvider{make(chan struct{}), make(chan struct{})}
			d.centralCodex = &centralCodex{provider: p}
			done := make(chan error, 1)
			go func() {
				tok, err := (codexTokenSource{d}).CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: "w", Refresh: true})
				if tok.AccessToken != "" {
					t.Error("late token disclosed")
				}
				done <- err
			}()
			<-p.started
			switch change {
			case "archive":
				db, _ := store.Open()
				db.SetArchivedFlag("w", true, store.Now())
				db.Close()
			case "revoke":
				d.authority.Revoke(t.Context(), access.SubjectSession, "w")
			case "logout":
				accounts.Update(func(s *accounts.State) error {
					p := s.Profiles["codex:central"]
					p.DeliveryEpoch++
					p.State = "logged-out"
					s.Profiles[p.ID] = p
					return nil
				})
			case "rebind":
				accounts.Update(func(s *accounts.State) error { b := s.Bindings["w"]; b.Revision++; s.Bindings["w"] = b; return nil })
			}
			close(p.release)
			if err := <-done; err == nil {
				t.Fatal("late refresh succeeded")
			}
		})
	}
}

func TestAccountsRevalidateHostAfterQueuedAdmission(t *testing.T) {
	d := isolatedAccounts(t)
	var revoked atomic.Bool
	ctx := withAccessGuard(t.Context(), func(context.Context) error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	r := hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), Initiator: "host"}
	raw, _ := json.Marshal(r)
	d.effectMu.Lock()
	done := make(chan any, 1)
	go func() {
		out, _ := d.readModelContext(ctx, core.Action{Action: core.ActionQuery, Query: core.QueryAccountCeremony, Fields: map[string]string{"request": string(raw)}})
		done <- out
	}()
	revoked.Store(true)
	d.effectMu.Unlock()
	select {
	case v := <-done:
		if out := v.(hp.AccountResult); out.OK || out.Error != "account_access_denied" {
			t.Fatal(out)
		}
	case <-time.After(time.Second):
		t.Fatal("queued admission stuck")
	}
	state, _ := accounts.Read()
	if state.Profiles[r.ProfileID].State == "login-pending" {
		t.Fatal("revoked host changed profile")
	}
}

func TestManagerStartupRestartAndConcurrentRebindDoNotCycleEffectLock(t *testing.T) {
	for _, mode := range []string{"startup-and-restart", "concurrent-rebind"} {
		t.Run(mode, func(t *testing.T) {
			row := store.Session{ID: "w", Agent: "codex", Dir: t.TempDir()}
			d := centralCodexDaemon(t, row)
			p := &blockingAccountProvider{make(chan struct{}), make(chan struct{})}
			d.centralCodex = &centralCodex{provider: p}
			d.codex.SetChatGPTAuth(codexTokenSource{d})
			endpoint, _ := newStructuredRPCServer(t)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ensure := func() error {
				_, err := d.codex.Ensure(ctx, "w", row.Dir, nil, []string{"sleep", "60"}, endpoint, "", "", "")
				return err
			}
			if mode == "startup-and-restart" {
				close(p.release)
				for range 2 {
					d.effectMu.Lock()
					done := make(chan error, 1)
					go func() { done <- ensure() }()
					select {
					case err := <-done:
						d.effectMu.Unlock()
						if err != nil {
							t.Fatal(err)
						}
					case <-ctx.Done():
						d.effectMu.Unlock()
						t.Fatal("manager startup waited for effectMu")
					}
					d.codex.Close("w")
					p.started = make(chan struct{})
				}
			} else {
				if err := accounts.Update(func(s *accounts.State) error {
					p := s.Profiles["codex:central"]
					p.State = "ready"
					p.Identity.AccountID = "account-a"
					s.Profiles[p.ID] = p
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				started := make(chan error, 1)
				go func() { started <- ensure() }()
				<-p.started
				rebound := make(chan hp.AccountResult, 1)
				go func() {
					rebound <- d.accountCall(ctx, hp.AccountRequest{Operation: hp.AccountRebind, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central"), WorkloadID: "w"}, false)
				}()
				close(p.release)
				select {
				case err := <-started:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("startup deadlocked against rebind")
				}
				select {
				case out := <-rebound:
					if !out.OK {
						t.Fatal(out)
					}
				case <-ctx.Done():
					t.Fatal("rebind deadlocked waiting startup gate")
				}
			}
		})
	}
}

func TestStartupRetiresOrphanedPendingProfiles(t *testing.T) {
	d := centralCodexDaemon(t)
	t.Setenv("AMUX_CODEX_BIN", "/missing-test-codex")
	accounts.Update(func(s *accounts.State) error {
		for id, p := range s.Profiles {
			if p.Source == "central" {
				p.State = "login-pending"
				s.Profiles[id] = p
			}
		}
		return nil
	})
	before, _ := accounts.Read()
	d.startCentralCodex(t.Context())
	defer d.closeCentralCodex()
	after, _ := accounts.Read()
	for id, p := range after.Profiles {
		if p.Source == "central" && (p.State != "blocked" || p.DeliveryEpoch <= before.Profiles[id].DeliveryEpoch) {
			t.Fatal(p)
		}
	}
	if len(d.accountAttempts) != 0 {
		t.Fatal("replayed orphan ceremony")
	}
}

type completionAccountProvider struct {
	blockingAccountProvider
	loginStarted, finish chan struct{}
	logoutErr            error
}

func (p *completionAccountProvider) Login(ctx context.Context, show func(string), commits ...func() error) error {
	if p.loginStarted != nil {
		close(p.loginStarted)
	}
	if p.finish != nil {
		select {
		case <-p.finish:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, commit := range commits {
		if err := commit(); err != nil {
			return err
		}
	}
	return nil
}
func (p *completionAccountProvider) Logout(context.Context) error { return p.logoutErr }
func TestNativeCompletionRevalidatesInitiatingHost(t *testing.T) {
	d := isolatedAccounts(t)
	p := &completionAccountProvider{loginStarted: make(chan struct{}), finish: make(chan struct{})}
	d.centralCodex = &centralCodex{provider: p}
	var revoked atomic.Bool
	ctx := withAccessGuard(t.Context(), func(context.Context) error {
		if revoked.Load() {
			return errors.New("revoked")
		}
		return nil
	})
	r := hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central"), Initiator: "host"}
	out := d.accountCall(ctx, r, true)
	if !out.OK {
		t.Fatal(out)
	}
	<-p.loginStarted
	revoked.Store(true)
	close(p.finish)
	deadline := time.Now().Add(time.Second)
	for {
		d.accountMu.Lock()
		state := d.accountAttempts[out.Ceremony.AttemptID].ceremony.State
		d.accountMu.Unlock()
		if state == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revoked completion not retired")
		}
		time.Sleep(time.Millisecond)
	}
	s, _ := accounts.Read()
	if s.Profiles[r.ProfileID].State == "ready" {
		t.Fatal("revoked native completion admitted")
	}
}
func TestLogoutCleanupErrorPersistsAndLoginRecovers(t *testing.T) {
	d := isolatedAccounts(t)
	p := &completionAccountProvider{logoutErr: errors.New("native secret error")}
	d.centralCodex = &centralCodex{provider: p}
	r := hp.AccountRequest{Operation: hp.AccountLogout, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central")}
	if out := d.accountCall(t.Context(), r, false); out.OK || out.Error != "account_logout_cleanup_failed" {
		t.Fatal(out)
	}
	snap, err := d.accountSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, profile := range snap.Profiles {
		if profile.ID == r.ProfileID {
			found = true
			if profile.State != "logged-out" || profile.Error != "account_logout_cleanup_failed" {
				t.Fatal(profile)
			}
		}
	}
	if !found {
		t.Fatal("missing profile")
	}
	accounts.Bind("w", "codex")
	if _, _, err := accounts.Delivery("w", "codex"); err == nil {
		t.Fatal("cleanup failure enabled delivery")
	}
	r.Operation = hp.AccountLogin
	r.Revision = profileRevision(t, r.ProfileID)
	r.Initiator = "host"
	out := d.accountCall(t.Context(), r, true)
	if !out.OK {
		t.Fatal(out)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s, _ := accounts.Read()
		profile := s.Profiles[r.ProfileID]
		if profile.State == "ready" {
			if profile.Error != "" {
				t.Fatal(profile)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("login did not recover")
		}
		time.Sleep(time.Millisecond)
	}
}
