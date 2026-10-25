// Package codexauth owns amux's single host-side Codex ChatGPT login. One
// managed native `codex app-server` (stdio transport) runs with a private,
// host-only CODEX_HOME beneath core.AuthDir(); it alone performs login,
// logout, and token refresh through the native account/* methods, so the
// rotating refresh-token chain has exactly one writer. amux never implements an
// OAuth endpoint itself.
//
// Sessions receive only the short-lived access token (plus account id and plan)
// for injection as in-memory chatgptAuthTokens. The narrow auth.json parser
// decodes only the fields named in Tokens: the refresh token never reaches a Go
// value, an error, a log line, or an event. Tokens redacts itself when printed
// or marshaled.
//
// Logout and re-login stop future refreshes but cannot revoke an access token a
// session already holds; it stays valid until its own expiry.
package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"amux/internal/core"
)

var (
	// ErrNotLoggedIn: the central home has no ChatGPT login. Run Login.
	ErrNotLoggedIn = errors.New("codexauth: no central Codex ChatGPT login; run `amux auth login codex`")
	// ErrUnsupportedAuth: the central home holds unsupported or malformed
	// credentials. Never handed to sessions.
	ErrUnsupportedAuth = errors.New("codexauth: unsupported central Codex credential")
	// ErrInsecureHome: the private home or auth.json is a symlink, not owned by
	// this user, or group/other accessible.
	ErrInsecureHome = errors.New("codexauth: central Codex home is not private")
	// ErrAccountChanged: the central login now belongs to a different ChatGPT
	// account than the session was bound to.
	ErrAccountChanged = errors.New("codexauth: central Codex login is a different ChatGPT account than this session's")
	// ErrBusy: another process (normally the daemon) owns the central native
	// server. Route login/logout through that owner, or stop it first.
	ErrBusy = errors.New("codexauth: central Codex auth is owned by another amux process; run it through the daemon or stop the daemon first")
	// ErrClosed: the Provider was closed.
	ErrClosed = errors.New("codexauth: provider closed")
	// ErrRefreshFailed: a native refresh ran but did not rotate the access
	// token while the login itself survived. Codex's account/read reports a
	// transient refresh failure (network, 5xx) only in its logs, keeping the
	// ChatGPT account and the old token; a permanent one (revoked refresh
	// token) clears the account and surfaces as ErrNotLoggedIn instead. Retry.
	ErrRefreshFailed = errors.New("codexauth: central Codex token refresh did not complete (likely transient); it is retried on the next request — run `amux auth login codex` only if it persists")
)

// Tokens is the session-safe projection of the central login. It has no
// refresh token field by construction.
type Tokens struct {
	AccessToken string
	APIKey      bool   // AccessToken is an API key; no verified account identity
	AccountID   string // chatgptAccountId
	PlanType    string // chatgptPlanType; may be empty
	// ExpiresAt is the access token's JWT exp, or zero when it has none.
	ExpiresAt time.Time
	// Generation increases each time the provider observes a different access
	// token; SessionToken uses it to coalesce concurrent refreshes.
	Generation uint64
}

func (t Tokens) String() string {
	return fmt.Sprintf("codexauth.Tokens{AccessToken:<redacted>, AccountID:%q, PlanType:%q, ExpiresAt:%s, Generation:%d}",
		t.AccountID, t.PlanType, t.ExpiresAt.Format(time.RFC3339), t.Generation)
}

func (t Tokens) GoString() string { return t.String() }

// MarshalJSON omits the access token, so an accidental log or event encoding
// cannot carry it. Callers deliver AccessToken explicitly.
func (t Tokens) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AccountID  string    `json:"account_id"`
		PlanType   string    `json:"plan_type,omitempty"`
		ExpiresAt  time.Time `json:"expires_at"`
		Generation uint64    `json:"generation"`
	}{t.AccountID, t.PlanType, t.ExpiresAt, t.Generation})
}

// Account is the non-secret login status reported by the native server.
type Account struct {
	Type     string // "chatgpt", "apiKey", or "" when logged out
	Email    string
	PlanType string
}

// Config configures a Provider. Only Argv is required.
type Config struct {
	// Argv is the resolved codex executable prefix (as agent.Argv("codex", "")
	// returns, without app-server arguments). Run on the host, never sandboxed.
	Argv []string
	// Home is the private CODEX_HOME. Default: Home().
	Home string
	// Env is the base environment. Default: os.Environ(). CODEX_HOME, API-key
	// and logging variables are always removed or overridden.
	Env []string
	// RefreshSkew refreshes a token this long before ExpiresAt. Default 5m.
	RefreshSkew time.Duration
	// Stderr optionally receives the native server's stderr, after redaction.
	Stderr io.Writer
	// Now is for tests. Default time.Now.
	Now func() time.Time
}

// Provider serializes every native account operation through one managed app
// server, started lazily and restarted after it exits. Safe for concurrent use.
type Provider struct {
	cfg Config

	// op (capacity 1) serializes native operations, and so refresh-token
	// rotation. Acquisition honors the caller's ctx so a long login or refresh
	// cannot push a session's bounded refresh past its deadline. Never held by
	// the cached fast path.
	op   chan struct{}
	srv  *server
	lock *os.File      // cross-process owner lock, held from first start until Close
	quit chan struct{} // closed by Close; interrupts acquisition, calls, and login

	mu      sync.Mutex
	cur     Tokens
	have    bool
	fetched time.Time
	closed  bool
	given   map[string]uint64 // session id -> generation last handed to it
}

// Home is the default host-only central Codex home: <AuthDir>/codex. Panes
// mask AuthDir, so no sandbox can reach it.
func Home() string { return filepath.Join(core.AuthDir(), "codex") }

// New validates cfg and creates (0700) the private home. It starts no process.
func New(cfg Config) (*Provider, error) {
	if len(cfg.Argv) == 0 || cfg.Argv[0] == "" {
		return nil, errors.New("codexauth: codex executable is required")
	}
	if cfg.Home == "" {
		cfg.Home = Home()
	}
	if !filepath.IsAbs(cfg.Home) {
		return nil, fmt.Errorf("codexauth: home %q is not absolute", cfg.Home)
	}
	cfg.Home = filepath.Clean(cfg.Home)
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Argv = append([]string(nil), cfg.Argv...)
	cfg.Env = append([]string(nil), cfg.Env...)
	if err := ensureHome(cfg.Home); err != nil {
		return nil, err
	}
	return &Provider{cfg: cfg, op: make(chan struct{}, 1), quit: make(chan struct{})}, nil
}

// fresh reports whether t can be handed out without a native refresh. A token
// without exp is reused for one skew interval after it was read.
func (p *Provider) fresh(t Tokens, fetched time.Time) bool {
	now := p.cfg.Now()
	if t.ExpiresAt.IsZero() {
		return now.Before(fetched.Add(p.cfg.RefreshSkew))
	}
	return now.Add(p.cfg.RefreshSkew).Before(t.ExpiresAt)
}

func (p *Provider) cached() (Tokens, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Tokens{}, false, ErrClosed
	}
	if p.have && p.fresh(p.cur, p.fetched) {
		return p.cur, true, nil
	}
	return Tokens{}, false, nil
}

// observe records parsed tokens, advancing the generation when they differ.
func (p *Provider) observe(t Tokens) Tokens {
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Generation = p.cur.Generation
	if !p.have || t.AccessToken != p.cur.AccessToken {
		t.Generation++
	}
	p.cur, p.have, p.fetched = t, true, p.cfg.Now()
	return t
}

func (p *Provider) acquire(ctx context.Context) error {
	select {
	case p.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("codexauth: waiting for another central auth operation: %w", ctx.Err())
	case <-p.quit:
		return ErrClosed
	}
}

func (p *Provider) release() { <-p.op }

func (p *Provider) forget() {
	p.mu.Lock()
	p.have = false
	p.cur.AccessToken = ""
	p.mu.Unlock()
}

// Tokens returns the current access token, refreshing through the native
// server (account/read refreshToken=true) only when it is within RefreshSkew of
// expiry. Concurrent callers share one refresh.
func (p *Provider) Tokens(ctx context.Context) (Tokens, error) {
	if t, ok, err := p.cached(); ok || err != nil {
		return t, err
	}
	if err := p.acquire(ctx); err != nil {
		return Tokens{}, err
	}
	defer p.release()
	if t, ok, err := p.cached(); ok || err != nil {
		return t, err
	}
	// The native server may already have refreshed and persisted; reuse that
	// without rotating again.
	if t, err := readTokens(p.cfg.Home); err == nil && p.fresh(t, p.cfg.Now()) {
		return p.observe(t), nil
	} else if err != nil && !errors.Is(err, ErrNotLoggedIn) && !errors.Is(err, errMalformed) {
		return Tokens{}, err
	}
	return p.refreshLocked(ctx)
}

// SessionToken serves one supervised session (the codexapp TokenProvider
// contract; the daemon shim maps TokenRequest onto these arguments). Without
// refresh it returns the current token. With refresh — the session's App Server
// got a 401 — it forces a real native refresh even if the token's exp is still
// in the future, unless another caller already rotated past the generation
// this session last received; serialized concurrent 401s therefore rotate
// once, not once per session. A non-empty previousAccountID must match the
// returned account (ErrAccountChanged otherwise), so a session never moves to
// another ChatGPT workspace.
func (p *Provider) SessionToken(ctx context.Context, sessionID string, refresh bool, previousAccountID string) (Tokens, error) {
	if sessionID == "" {
		return Tokens{}, errors.New("codexauth: session id is required")
	}
	var t Tokens
	var err error
	if refresh {
		t, err = p.forceRefresh(ctx, sessionID)
	} else {
		t, err = p.Tokens(ctx)
	}
	if err != nil {
		return Tokens{}, err
	}
	if previousAccountID != "" && t.AccountID != previousAccountID {
		return Tokens{}, ErrAccountChanged
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return Tokens{}, ErrClosed
	}
	if p.given == nil {
		p.given = map[string]uint64{}
	}
	p.given[sessionID] = t.Generation
	return t, nil
}

func (p *Provider) forceRefresh(ctx context.Context, sessionID string) (Tokens, error) {
	if err := p.acquire(ctx); err != nil {
		return Tokens{}, err
	}
	defer p.release()
	p.mu.Lock()
	cur, have, closed := p.cur, p.have, p.closed
	given, seen := p.given[sessionID]
	p.mu.Unlock()
	if closed {
		return Tokens{}, ErrClosed
	}
	if have && seen && cur.Generation > given && p.usable(cur) {
		return cur, nil
	}
	t, err := p.refreshLocked(ctx)
	if err != nil {
		return Tokens{}, err
	}
	if have && t.AccessToken == cur.AccessToken {
		return Tokens{}, ErrRefreshFailed
	}
	return t, nil
}

// usable reports whether a token is not yet expired (skew aside): a newer
// token another session rotated to is still worth handing out.
func (p *Provider) usable(t Tokens) bool {
	return t.ExpiresAt.IsZero() || p.cfg.Now().Before(t.ExpiresAt)
}

// ForgetSession drops a stopped or archived session's coalescing state. Tokens
// already delivered stay valid until they expire; this only bounds the map.
func (p *Provider) ForgetSession(sessionID string) {
	p.mu.Lock()
	delete(p.given, sessionID)
	p.mu.Unlock()
}

// Start launches the native server ahead of the first refresh, so a session's
// bounded refresh does not also pay process startup.
func (p *Provider) Start(ctx context.Context) error {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	_, err := p.serverLocked(ctx)
	return err
}

// refreshLocked performs one native refresh, then reads what it persisted.
// Caller holds p.op.
func (p *Provider) refreshLocked(ctx context.Context) (Tokens, error) {
	acct, err := p.readAccountLocked(ctx, true)
	if err != nil {
		return Tokens{}, err
	}
	switch acct.Type {
	case "":
		p.forget()
		return Tokens{}, ErrNotLoggedIn
	case "chatgpt", "apiKey":
	default:
		p.forget()
		return Tokens{}, ErrUnsupportedAuth
	}
	t, err := readTokens(p.cfg.Home)
	if errors.Is(err, errMalformed) {
		// A concurrent native write may be mid-flight; read once more.
		time.Sleep(50 * time.Millisecond)
		t, err = readTokens(p.cfg.Home)
	}
	if err != nil {
		if errors.Is(err, errMalformed) {
			err = ErrUnsupportedAuth
		}
		p.forget()
		return Tokens{}, err
	}
	if !t.ExpiresAt.IsZero() && !p.cfg.Now().Before(t.ExpiresAt) {
		p.forget()
		// The account survived, so the refresh failed transiently (see
		// ErrRefreshFailed) and left the expired token in place.
		return Tokens{}, ErrRefreshFailed
	}
	if t.PlanType == "" {
		t.PlanType = acct.PlanType
	}
	return p.observe(t), nil
}

// Status reports the login without refreshing it.
func (p *Provider) Status(ctx context.Context) (Account, error) {
	if err := p.acquire(ctx); err != nil {
		return Account{}, err
	}
	defer p.release()
	return p.readAccountLocked(ctx, false)
}

func (p *Provider) readAccountLocked(ctx context.Context, refresh bool) (Account, error) {
	srv, err := p.serverLocked(ctx)
	if err != nil {
		return Account{}, err
	}
	raw, err := srv.call(ctx, "account/read", map[string]any{"refreshToken": refresh})
	if err != nil {
		return Account{}, err
	}
	var res struct {
		Account *struct {
			Type     string `json:"type"`
			Email    string `json:"email"`
			PlanType string `json:"planType"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return Account{}, fmt.Errorf("codexauth: account/read: unexpected response")
	}
	if res.Account == nil {
		return Account{}, nil
	}
	return Account{Type: res.Account.Type, Email: res.Account.Email, PlanType: res.Account.PlanType}, nil
}

// Login runs the native ChatGPT browser login (account/login/start). show is
// called once with the authorization URL; Login returns when the native
// account/login/completed notification arrives or ctx ends (the login is then
// cancelled natively).
var ErrLoginCleanup = errors.New("codexauth: cancelled login cleanup failed")

func (p *Provider) Login(ctx context.Context, show func(authURL string), commit ...func() error) error {
	return p.login(ctx, "", show, commit...)
}

// LoginAPIKey uses the same native owner and rollback transaction as browser
// login. The key travels only over private stdio, never argv or the environment.
func (p *Provider) LoginAPIKey(ctx context.Context, key string, commit ...func() error) error {
	if !validToken(key) || len(key) > 4096 {
		return errors.New("codexauth: invalid API key")
	}
	return p.login(ctx, key, nil, commit...)
}

func (p *Provider) login(ctx context.Context, key string, show func(string), commit ...func() error) (resultErr error) {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	srv, err := p.serverLocked(ctx)
	if err != nil {
		return err
	}
	prior, existed, err := p.snapshotAuth()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		// Reap the native writer before restoring a cancelled attempt. Neither a
		// late callback nor an outstanding native task can rewrite the restored file.
		if resultErr == nil && !committed {
			resultErr = ctx.Err()
		}
		if resultErr != nil {
			_ = srv.stop()
			p.srv = nil
			p.forget()
			if restoreErr := p.restoreAuth(prior, existed); restoreErr != nil {
				resultErr = ErrLoginCleanup
			}
		}
	}()
	completed := srv.subscribe("account/login/completed")
	defer srv.unsubscribe("account/login/completed")
	params := map[string]any{"type": "chatgpt"}
	if key != "" {
		params = map[string]any{"type": "apiKey", "apiKey": key}
	}
	raw, err := srv.call(ctx, "account/login/start", params)
	if err != nil {
		return errors.New("codexauth: native login request failed")
	}
	if key != "" {
		var response struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Type != "apiKey" {
			return errors.New("codexauth: unexpected API login response")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Verify the protected native record before making it deliverable.
		token, err := readTokens(p.cfg.Home)
		if err != nil || !token.APIKey || token.AccessToken != key {
			return errors.New("codexauth: API key was not stored")
		}
		// Invalidate the old cache before metadata admission can expose a new
		// epoch. Concurrent token readers must wait for this operation's release.
		p.forget()
		for _, admit := range commit {
			if err := admit(); err != nil {
				return err
			}
		}
		committed = true
		return nil
	}
	var start struct {
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(raw, &start); err != nil || start.LoginID == "" || !validLoginURL(start.AuthURL) {
		return errors.New("codexauth: account/login/start: unexpected response")
	}
	if show != nil {
		show(start.AuthURL)
	}
	for {
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = srv.call(cancelCtx, "account/login/cancel", map[string]any{"loginId": start.LoginID})
			cancel()
			return ctx.Err()
		case <-p.quit:
			return ErrClosed
		case <-srv.done:
			return fmt.Errorf("codexauth: native server exited during login: %w", srv.exitErr())
		case params := <-completed:
			var done struct {
				LoginID string  `json:"loginId"`
				Success bool    `json:"success"`
				Error   *string `json:"error"`
			}
			if json.Unmarshal(params, &done) != nil || done.LoginID != start.LoginID {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			p.forget()
			if !done.Success {
				msg := "login failed"
				if done.Error != nil && *done.Error != "" {
					msg = redact(*done.Error)
				}
				return fmt.Errorf("codexauth: native login: %s", msg)
			}
			for _, admit := range commit {
				if err := admit(); err != nil {
					return err
				}
			}
			committed = true
			return nil
		}
	}
}

// Logout signs the central home out natively and drops cached tokens.
func (p *Provider) Logout(ctx context.Context) error {
	if err := p.acquire(ctx); err != nil {
		return err
	}
	defer p.release()
	srv, err := p.serverLocked(ctx)
	if err != nil {
		return err
	}
	p.forget()
	_, err = srv.call(ctx, "account/logout", map[string]any{})
	_ = srv.stop()
	p.srv = nil
	p.forget()
	if cleanupErr := p.restoreAuth(nil, false); cleanupErr != nil {
		return errors.New("codexauth: logout cleanup failed")
	}
	return err
}

// Close stops the managed server (its whole process group) and waits for it.
func (p *Provider) Close() error {
	p.mu.Lock()
	already := p.closed
	p.closed = true
	p.have = false
	p.cur.AccessToken = ""
	p.given = nil
	p.mu.Unlock()
	if !already {
		close(p.quit)
	}
	p.op <- struct{}{}
	defer p.release()
	var err error
	if p.srv != nil {
		err = p.srv.stop()
		p.srv = nil
	}
	if p.lock != nil {
		_ = p.lock.Close() // releases the owner flock
		p.lock = nil
	}
	return err
}

// serverLocked returns a live native server, starting or restarting it.
// Caller holds p.op.
func (p *Provider) serverLocked(ctx context.Context) (*server, error) {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, ErrClosed
	}
	if p.srv != nil && p.srv.alive() {
		return p.srv, nil
	}
	if p.srv != nil {
		_ = p.srv.stop()
		p.srv = nil
	}
	if err := ensureHome(p.cfg.Home); err != nil {
		return nil, err
	}
	if p.lock == nil {
		lock, err := ownerLock(p.cfg.Home)
		if err != nil {
			return nil, err
		}
		p.lock = lock
	}
	srv, err := startServer(ctx, p.cfg, p.quit)
	if err != nil {
		return nil, err
	}
	p.srv = srv
	return srv, nil
}
