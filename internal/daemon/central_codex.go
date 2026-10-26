package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// codexLoginTimeout bounds one interactive central Codex login.
const codexLoginTimeout = 15 * time.Minute

var errCentralCodexUnavailable = errors.New("central Codex auth is unavailable in this daemon; see amux auth status codex")

// centralCodex is the daemon's single owner of the central Codex home. Only
// the daemon (or, with no daemon running, the host CLI) may own it; the
// provider's cross-process lock enforces that.
type centralCodex struct {
	provider centralCodexProvider
	err      error // why provider is nil in central mode
	lifetime context.Context
}

// centralCodexProvider is the native boundary; production uses codexauth.Provider.
type centralCodexProvider interface {
	Start(context.Context) error
	Close() error
	ForgetSession(string)
	SessionToken(context.Context, string, bool, string) (codexauth.Tokens, error)
	Login(context.Context, func(string), ...func() error) error
	Logout(context.Context) error
	Status(context.Context) (codexauth.Account, error)
	RateLimits(context.Context) (harnessproto.AccountLimits, error)
}

// codexExecutable is the host codex binary: AMUX_CODEX_BIN or codex on PATH.
// It is the bare executable prefix; TUI flags from agent.Argv do not apply to
// the host-only app server.
func codexExecutable() (string, error) {
	bin := os.Getenv("AMUX_CODEX_BIN")
	if bin == "" {
		bin = "codex"
	}
	return exec.LookPath(bin)
}

// startCentralCodex creates and warms the provider when codex.auth is central
// and routes every supervised Codex App Server through it. A failure leaves
// the routing in place with a provider that refuses, so a central session
// fails with guidance instead of falling back to a per-session login.
func (d *Daemon) startCentralCodex(ctx context.Context) {
	if d.codex == nil {
		return
	}
	_ = accounts.Update(func(s *accounts.State) error {
		for id, p := range s.Profiles {
			if p.State == "login-pending" {
				p.State = "blocked"
				p.DeliveryEpoch++
				p.Revision++
				s.Profiles[id] = p
			}
		}
		return nil
	})
	c := &centralCodex{lifetime: ctx}
	d.centralCodex = c
	d.codex.SetChatGPTAuth(codexTokenSource{d: d})
	bin, err := codexExecutable()
	if err == nil {
		c.provider, err = codexauth.New(codexauth.Config{Argv: []string{bin}, Home: codexauth.Home()})
	}
	if err != nil {
		c.err = err
		log.Printf("amux: central Codex auth unavailable: %v", err)
		return
	}
	go func() {
		if err := c.provider.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("amux: central Codex auth: %v", err)
		}
	}()
}

// closeCentralCodex stops the provider's native server at daemon shutdown.
func (d *Daemon) closeCentralCodex() {
	d.disconnectAccounts("", true)
	if c := d.centralCodex; c != nil && c.provider != nil {
		_ = c.provider.Close()
	}
}

// closeCodex stops a session's supervisor and drops its central refresh state.
func (d *Daemon) closeCodex(id string) {
	if d.codex != nil {
		d.codex.Close(id)
	}
	if c := d.centralCodex; c != nil && c.provider != nil {
		c.provider.ForgetSession(id)
	}
}

// codexTokenSource is the codexapp.TokenProvider for central mode. The session
// id comes from the daemon's own supervisor, and must name a live Codex
// session before the central provider is consulted.
type codexTokenSource struct{ d *Daemon }

func (s codexTokenSource) CodexChatGPTToken(ctx context.Context, req codexapp.TokenRequest) (codexapp.ChatGPTToken, error) {
	if !s.authorized(req.SessionID) {
		return codexapp.ChatGPTToken{}, errors.New("central Codex auth: session not authorized")
	}
	c := s.d.centralCodex
	if c == nil || c.provider == nil {
		return codexapp.ChatGPTToken{}, errCentralCodexUnavailable
	}
	binding, profile, err := accounts.Delivery(req.SessionID, "codex")
	if err != nil {
		return codexapp.ChatGPTToken{}, err
	}
	if s.d.authority == nil {
		return codexapp.ChatGPTToken{}, errors.New("central Codex auth: authority unavailable")
	}
	credential, err := access.LoadCredential(s.d.authority.CredentialDir(access.SubjectSession, req.SessionID))
	if err != nil {
		return codexapp.ChatGPTToken{}, errors.New("central Codex auth: session not authorized")
	}
	principal := access.Principal{Kind: credential.Kind, SubjectID: credential.SubjectID, KeyID: credential.KeyID, Generation: credential.Generation}
	if err = s.d.authority.Valid(ctx, principal); err != nil {
		return codexapp.ChatGPTToken{}, err
	}
	// A superseded process never receives a credential, and a refresh is only
	// for the process that the binding currently pins.
	if req.Launch < binding.Launch || (req.Refresh && req.Launch != binding.Launch) {
		return codexapp.ChatGPTToken{}, accounts.ErrSuperseded
	}
	// The current process keeps its account; a newer launch takes the current
	// central login (another account continues the same thread).
	previous := req.PreviousAccountID
	if req.Launch == binding.Launch && binding.DeliveredEpoch != 0 && binding.ExpectedAccount != "" {
		if previous != "" && previous != binding.ExpectedAccount {
			return codexapp.ChatGPTToken{}, codexauth.ErrAccountChanged
		}
		previous = binding.ExpectedAccount
	}
	if profile.Mode == "api" && req.Refresh {
		return codexapp.ChatGPTToken{}, errors.New("API keys do not support subscription refresh")
	}
	t, err := c.provider.SessionToken(ctx, req.SessionID, req.Refresh, previous)
	if err != nil {
		return codexapp.ChatGPTToken{}, err
	}
	if t.APIKey != (profile.Mode == "api") {
		return codexapp.ChatGPTToken{}, accounts.ErrBlocked
	}
	if t.APIKey {
		state, err := accounts.Read()
		if err != nil {
			return codexapp.ChatGPTToken{}, err
		}
		t.Generation = state.CredentialVersion[profile.ID]
		if t.Generation == 0 {
			return codexapp.ChatGPTToken{}, accounts.ErrBlocked
		}
	}
	// Startup and Close hold the manager's per-session gate, sometimes below
	// effectMu. Delivery must never acquire effectMu: that creates a cycle.
	// The authority read lease plus metadata CAS is the delivery admission point.
	err = accounts.AcceptAuthorized(req.SessionID, binding.Revision, profile.DeliveryEpoch, t.AccountID, t.Generation, func(commit func() error) error {
		return s.d.authority.WithValid(ctx, principal, func() error {
			if !s.authorized(req.SessionID) {
				return errors.New("central Codex auth: session not authorized")
			}
			return commit()
		})
	}, req.Launch)
	if err != nil {
		return codexapp.ChatGPTToken{}, err
	}
	return codexapp.ChatGPTToken{AccessToken: t.AccessToken, APIKey: t.APIKey, AccountID: t.AccountID, PlanType: t.PlanType}, nil
}

// authorized reads the session without wsops.ResolveSession's create/migrate
// side effects: it must exist, be live, and run Codex.
func (s codexTokenSource) authorized(id string) bool {
	db, err := store.Open()
	if err != nil {
		return false
	}
	defer db.Close()
	session, ok, err := lookupPolicySession(db, id)
	return err == nil && ok && !session.Archived && agent.Canonical(session.Agent) == "codex"
}

// codexLogin starts the daemon-owned interactive login, or cancels it. A login
// already pending is left running so a retried CLI shows the same URL.
func (d *Daemon) codexLogin(a core.Action) error {
	if a.ID != "" || a.Target != "" {
		return fmt.Errorf("codex-login takes no target")
	}
	for k, v := range a.Fields {
		if k != "cancel" || (v != "true" && v != "false") {
			return fmt.Errorf("codex-login accepts only cancel=true|false")
		}
	}
	if d.centralCodex == nil || d.centralCodex.provider == nil {
		return errCentralCodexUnavailable
	}
	return errors.New("account_ceremony_required")
}

// codexAuthStatus is the QueryCodexAuth read model.
func (d *Daemon) codexAuthStatus(ctx context.Context) core.CodexAuthStatus {
	st := core.CodexAuthStatus{Mode: codexcfg.AuthMode(), Login: core.CodexLoginStatus{State: "idle"}}
	c := d.centralCodex
	if c == nil {
		if st.Mode == amuxcfg.AuthCentral {
			st.Error = "this daemon started before codex.auth was central; run `amux daemon restart`"
		}
		return st
	}
	d.accountMu.Lock()
	for _, a := range d.accountAttempts {
		if a.initiator == "legacy-host" {
			st.Login = core.CodexLoginStatus{State: a.ceremony.State}
			if st.Login.State == "failed" {
				st.Login.Error = "account_login_failed"
			}
		}
	}
	d.accountMu.Unlock()
	if c.provider == nil {
		st.Error = "account_native_unavailable"
		return st
	}
	if st.Login.State == "pending" {
		return st // the native server is busy with the login
	}
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	account, err := c.provider.Status(statusCtx)
	if err != nil {
		st.Error = "account_native_unavailable"
		return st
	}
	st.Account = core.CodexAccount{Type: account.Type, Email: account.Email, PlanType: account.PlanType}
	return st
}
