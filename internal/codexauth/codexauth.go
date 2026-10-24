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
package codexauth

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	// ErrNotLoggedIn: the central home has no ChatGPT login. Run Login.
	ErrNotLoggedIn = errors.New("codexauth: no central Codex ChatGPT login; run amux auth codex login")
	// ErrUnsupportedAuth: the central home holds a non-ChatGPT credential (an API
	// key) or malformed tokens. Never handed to sessions.
	ErrUnsupportedAuth = errors.New("codexauth: central Codex login is not a ChatGPT login")
	// ErrInsecureHome: the private home or auth.json is a symlink, not owned by
	// this user, or group/other accessible.
	ErrInsecureHome = errors.New("codexauth: central Codex home is not private")
	// ErrClosed: the Provider was closed.
	ErrClosed = errors.New("codexauth: provider closed")
)

// Tokens is the session-safe projection of the central login. It has no
// refresh token field by construction.
type Tokens struct {
	AccessToken string
	AccountID   string // chatgptAccountId
	PlanType    string // chatgptPlanType; may be empty
	// ExpiresAt is the access token's JWT exp, or zero when it has none.
	ExpiresAt time.Time
	// Generation increases each time the provider observes a different access
	// token. Pass it back to Refresh so concurrent stale callers coalesce.
	Generation uint64
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
type Provider struct{}

// Home is the default host-only central Codex home: <AuthDir>/codex. Panes
// mask AuthDir, so no sandbox can reach it.
func Home() string { panic("unimplemented") }

// New validates cfg and creates (0700) the private home. It starts no process.
func New(cfg Config) (*Provider, error) { panic("unimplemented") }

// Tokens returns the current access token, refreshing through the native
// server (account/read refreshToken=true) only when it is within RefreshSkew of
// expiry. Concurrent callers share one refresh.
func (p *Provider) Tokens(ctx context.Context) (Tokens, error) { panic("unimplemented") }

// Refresh answers a session's account/chatgptAuthTokens/refresh. If the
// provider already holds a newer generation than stale, it returns that without
// another rotation; otherwise it forces exactly one native refresh.
func (p *Provider) Refresh(ctx context.Context, stale uint64) (Tokens, error) {
	panic("unimplemented")
}

// Status reports the login without refreshing it.
func (p *Provider) Status(ctx context.Context) (Account, error) { panic("unimplemented") }

// Login runs the native ChatGPT browser login (account/login/start). show is
// called once with the authorization URL; Login returns when the native
// account/login/completed notification arrives or ctx ends (the login is then
// cancelled natively).
func (p *Provider) Login(ctx context.Context, show func(authURL string)) error {
	panic("unimplemented")
}

// Logout signs the central home out natively and drops cached tokens.
func (p *Provider) Logout(ctx context.Context) error { panic("unimplemented") }

// Close stops the managed server (its whole process group) and waits for it.
func (p *Provider) Close() error { panic("unimplemented") }
