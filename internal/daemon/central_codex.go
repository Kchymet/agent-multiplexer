package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/codexcfg"
	"amux/internal/core"
)

// codexLoginTimeout bounds one interactive central Codex login.
const codexLoginTimeout = 15 * time.Minute

var errCentralCodexUnavailable = errors.New("central Codex auth is unavailable in this daemon; see amux auth status codex")

// centralCodex is the daemon's single owner of the central Codex home. Only
// the daemon (or, with no daemon running, the host CLI) may own it; the
// provider's cross-process lock enforces that.
type centralCodex struct {
	provider *codexauth.Provider
	err      error // why provider is nil in central mode
	lifetime context.Context

	mu     sync.Mutex
	login  core.CodexLoginStatus
	cancel context.CancelFunc
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
	if d.codex == nil || codexcfg.AuthMode() != amuxcfg.AuthCentral {
		return
	}
	c := &centralCodex{lifetime: ctx, login: core.CodexLoginStatus{State: "idle"}}
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
	if c := d.centralCodex; c != nil && c.provider != nil {
		c.mu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		c.mu.Unlock()
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
	session, ok, err := lookupSession(req.SessionID)
	if err != nil || !ok || session.Archived || agent.Canonical(session.Agent) != "codex" {
		return codexapp.ChatGPTToken{}, errors.New("central Codex auth: session not authorized")
	}
	c := s.d.centralCodex
	if c == nil || c.provider == nil {
		return codexapp.ChatGPTToken{}, errCentralCodexUnavailable
	}
	t, err := c.provider.SessionToken(ctx, req.SessionID, req.Refresh, req.PreviousAccountID)
	if err != nil {
		return codexapp.ChatGPTToken{}, err
	}
	return codexapp.ChatGPTToken{AccessToken: t.AccessToken, AccountID: t.AccountID, PlanType: t.PlanType}, nil
}

// codexLogin starts the daemon-owned interactive login, or cancels it. A login
// already pending is left running so a retried CLI shows the same URL.
func (d *Daemon) codexLogin(a core.Action) error {
	for k, v := range a.Fields {
		if k != "cancel" || (v != "true" && v != "false") {
			return fmt.Errorf("codex-login accepts only cancel=true|false")
		}
	}
	if a.ID != "" || a.Target != "" {
		return fmt.Errorf("codex-login takes no target")
	}
	c := d.centralCodex
	if c == nil {
		return fmt.Errorf("this daemon did not start with codex.auth central; run `amux config unset codex.auth` and `amux daemon restart`")
	}
	if c.provider == nil {
		return fmt.Errorf("%w: %v", errCentralCodexUnavailable, c.err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.Fields["cancel"] == "true" {
		if c.cancel != nil {
			c.cancel()
		}
		return nil
	}
	if c.login.State == "pending" {
		return nil
	}
	ctx, cancel := context.WithTimeout(c.lifetime, codexLoginTimeout)
	c.cancel = cancel
	c.login = core.CodexLoginStatus{State: "pending"}
	go func() {
		defer cancel()
		err := c.provider.Login(ctx, func(url string) {
			c.mu.Lock()
			c.login.URL = url
			c.mu.Unlock()
		})
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cancel = nil
		switch {
		case err == nil:
			c.login = core.CodexLoginStatus{State: "succeeded"}
		case errors.Is(err, context.Canceled):
			c.login = core.CodexLoginStatus{State: "cancelled"}
		default:
			c.login = core.CodexLoginStatus{State: "failed", Error: err.Error()}
		}
	}()
	return nil
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
	c.mu.Lock()
	st.Login = c.login
	c.mu.Unlock()
	if c.provider == nil {
		st.Error = c.err.Error()
		return st
	}
	if st.Login.State == "pending" {
		return st // the native server is busy with the login
	}
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	account, err := c.provider.Status(statusCtx)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	st.Account = core.CodexAccount{Type: account.Type, Email: account.Email, PlanType: account.PlanType}
	return st
}
