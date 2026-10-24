package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// auth.go injects centrally owned ChatGPT credentials into a supervised App
// Server, so a structured session authenticates without any per-session
// auth.json. The daemon's provider holds the only refresh-capable login; each
// App Server receives just an access token, kept in memory, via the App
// Server's externally managed token mode (docs: developers.openai.com/codex/
// app-server, "Log in with externally managed ChatGPT tokens"):
//
//   - after initialize, account/login/start {type:"chatgptAuthTokens",
//     accessToken, chatgptAccountId, chatgptPlanType} installs the token;
//   - on a backend 401 the server sends the request
//     account/chatgptAuthTokens/refresh {reason, previousAccountId} and retries
//     once the client answers {accessToken, chatgptAccountId, chatgptPlanType}.
//     The server abandons the request after about 10 seconds.
//
// The public docs call this mode experimental (it needs capabilities.
// experimentalApi, which the handshake already sets). Upstream source also
// annotates it "[UNSTABLE] FOR OPENAI INTERNAL USE ONLY"
// (codex-rs/app-server-protocol/src/protocol/v2/account.rs), so an App Server
// that rejects it fails the launch with errExternalAuthUnsupported rather than
// silently falling back to whatever credentials the process can find.
//
// The tokens live only in the App Server's memory and are dropped with its
// connection state, so every handshake (fresh start or reconnect) injects
// again before any thread RPC. Nothing here emits a token: the login params
// never reach the event stream, the refresh request is consumed rather than
// passed through as raw, and errors carry fixed text plus a JSON-RPC code.

// refreshTimeout bounds one provider refresh, leaving margin under the App
// Server's own ~10s wait so the reply still arrives in time to be used.
// A variable only so tests can shorten it.
var refreshTimeout = 8 * time.Second

// loginCompletionTimeout bounds the wait for account/login/completed. Codex
// gives its own workspace-routing discovery 15s, so this leaves it room to
// report rather than amux guessing first. A variable only for tests.
var loginCompletionTimeout = 20 * time.Second

const (
	methodLoginStart   = "account/login/start"
	methodTokenRefresh = "account/chatgptAuthTokens/refresh"
	loginTypeTokens    = "chatgptAuthTokens"
)

// ChatGPTToken is one externally managed ChatGPT credential. AccessToken is a
// secret: String, GoString and MarshalJSON redact it, so an accidental log or
// event of the value never carries it.
type ChatGPTToken struct {
	AccessToken string
	AccountID   string // ChatGPT workspace/account id (chatgptAccountId)
	PlanType    string // optional; "" lets Codex derive it from the token
}

func (t ChatGPTToken) String() string {
	return fmt.Sprintf("ChatGPTToken{account=%q plan=%q token=<redacted>}", t.AccountID, t.PlanType)
}

func (t ChatGPTToken) GoString() string { return t.String() }

func (t ChatGPTToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"accountId": t.AccountID, "planType": t.PlanType, "accessToken": "<redacted>"})
}

// TokenRequest describes why the supervisor needs a token.
type TokenRequest struct {
	SessionID string
	// Refresh is true when the App Server reported 401 for the token it holds;
	// the provider must return a newer token rather than the cached one.
	Refresh bool
	// PreviousAccountID is the account the App Server was using (from the
	// refresh request, or the account injected earlier); "" on first injection.
	PreviousAccountID string
}

// TokenProvider supplies centrally owned ChatGPT tokens. Implementations must
// honour ctx (refreshes are bounded by refreshTimeout), deduplicate concurrent
// refreshes across sessions, and return errors that contain no secret.
type TokenProvider interface {
	CodexChatGPTToken(ctx context.Context, req TokenRequest) (ChatGPTToken, error)
}

var (
	// errExternalAuthUnsupported means the App Server rejected externally
	// managed tokens: a Codex build without (or with a changed) chatgptAuthTokens.
	errExternalAuthUnsupported = errors.New("codexapp: this codex app-server does not support externally managed ChatGPT tokens (account/login/start chatgptAuthTokens); upgrade Codex or disable central ChatGPT auth")
	// errAccountChanged refuses a refresh that would move a session to another
	// ChatGPT workspace mid-thread.
	errAccountChanged = errors.New("codexapp: central ChatGPT auth returned a different account than this session uses")
	// errNotInjected refuses a refresh request before amux installed a token:
	// there is no account to refresh for, so nothing is handed out.
	errNotInjected = errors.New("codexapp: no central ChatGPT token was injected")
)

// tokenAuth is a supervisor's view of central auth: the provider plus the
// account this App Server was bound to, and the in-flight refresh shared by
// concurrent 401s.
type tokenAuth struct {
	provider  TokenProvider
	sessionID string

	mu       sync.Mutex
	account  string
	injected bool // the App Server holds a token amux installed
	inflight *tokenRefresh
	// completed receives the outcome of the pending token login. Codex
	// acknowledges account/login/start before it finishes the login (it then
	// discovers the workspace's routing), and reports the result in a later
	// account/login/completed notification.
	completed chan loginOutcome
	// pending is the token being installed, kept only so a server message
	// that echoes it can be withheld.
	pending string
}

type loginOutcome struct {
	success bool
	err     string
}

type tokenRefresh struct {
	done chan struct{}
	tok  ChatGPTToken
	err  error
}

func newTokenAuth(p TokenProvider, sessionID, account string) *tokenAuth {
	if p == nil {
		return nil
	}
	return &tokenAuth{provider: p, sessionID: sessionID, account: account}
}

// boundAccount is the account injection established ("" before, or when the
// session does not use central auth).
func (a *tokenAuth) boundAccount() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.account
}

func (a *tokenAuth) fetch(ctx context.Context, refresh bool, previous string) (ChatGPTToken, error) {
	tok, err := a.provider.CodexChatGPTToken(ctx, TokenRequest{SessionID: a.sessionID, Refresh: refresh, PreviousAccountID: previous})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ChatGPTToken{}, fmt.Errorf("codexapp: central ChatGPT auth: %w", ctxErr)
		}
		return ChatGPTToken{}, fmt.Errorf("codexapp: central ChatGPT auth: %w", err)
	}
	if strings.TrimSpace(tok.AccessToken) == "" || strings.TrimSpace(tok.AccountID) == "" {
		return ChatGPTToken{}, errors.New("codexapp: central ChatGPT auth returned an incomplete credential")
	}
	return tok, nil
}

// login installs a token on a freshly initialized App Server. It runs on every
// handshake, so a reconnect re-establishes auth before any thread RPC. The
// account is bound by the first login (or by the resumed thread's persisted
// identity) and never changes afterwards.
func (a *tokenAuth) login(ctx context.Context, rpc *rpcConn) error {
	a.mu.Lock()
	previous := a.account
	a.mu.Unlock()
	fetchCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	tok, err := a.fetch(fetchCtx, false, previous)
	cancel()
	if err != nil {
		return err
	}
	if previous != "" && tok.AccountID != previous {
		return errAccountChanged
	}
	params := map[string]any{
		"type":             loginTypeTokens,
		"accessToken":      tok.AccessToken,
		"chatgptAccountId": tok.AccountID,
	}
	if tok.PlanType != "" {
		params["chatgptPlanType"] = tok.PlanType
	}
	completed := make(chan loginOutcome, 1)
	a.mu.Lock()
	a.completed = completed
	a.pending = tok.AccessToken
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.completed == completed {
			a.completed = nil
			a.pending = ""
		}
		a.mu.Unlock()
	}()
	res, err := rpc.call(ctx, methodLoginStart, params)
	if err != nil {
		return loginError(err)
	}
	var r struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(res, &r) != nil || r.Type != loginTypeTokens {
		return errExternalAuthUnsupported
	}
	// The server holds the token from here on, so a 401 during its own login
	// work (workspace discovery) may already ask for a refresh.
	a.mu.Lock()
	a.account = tok.AccountID
	a.injected = true
	a.mu.Unlock()

	wait := time.NewTimer(loginCompletionTimeout)
	defer wait.Stop()
	var out loginOutcome
	select {
	case out = <-completed:
	case <-wait.C:
		out = loginOutcome{err: "no account/login/completed within " + loginCompletionTimeout.String()}
	case <-ctx.Done():
		out = loginOutcome{err: ctx.Err().Error()}
	}
	if !out.success {
		a.mu.Lock()
		a.injected = false
		a.mu.Unlock()
		return fmt.Errorf("codexapp: App Server did not complete the central ChatGPT login: %s", safeServerText(out.err, tok.AccessToken))
	}
	return nil
}

// loginCompleted consumes an account/login/completed for the token login
// (loginId null): it delivers the outcome to the pending login and returns
// the redacted notice that replaces the raw notification, whose error text
// is server-supplied. ok is false for any other login (a native client's
// browser login carries a loginId), which passes through unchanged.
func (a *tokenAuth) loginCompleted(params json.RawMessage) (ev harnessproto.RuntimeEvent, ok bool) {
	var p struct {
		LoginID *string `json:"loginId"`
		Success bool    `json:"success"`
		Error   *string `json:"error"`
	}
	if a == nil || json.Unmarshal(params, &p) != nil || p.LoginID != nil {
		return ev, false
	}
	out := loginOutcome{success: p.Success}
	if p.Error != nil {
		out.err = *p.Error
	}
	a.mu.Lock()
	ch, pending := a.completed, a.pending
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- out:
		default:
		}
	}
	if out.success {
		return notice("info", "codex: central ChatGPT login completed"), true
	}
	return notice("error", "codex: central ChatGPT login failed: "+safeServerText(out.err, pending)), true
}

// safeServerText bounds a server-supplied login error for amux's own error
// text. Codex reduces these to structured reasons, but a token is never
// repeated even if one were echoed.
func safeServerText(text, token string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "unknown reason"
	}
	if token != "" && strings.Contains(text, token) || strings.Contains(text, "eyJ") {
		return "reason withheld (contained credential material)"
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// loginError maps a failed account/login/start onto a token-free error. The
// server's message is inspected but never repeated: a parse error could quote
// the rejected params.
func loginError(err error) error {
	var re *rpcError
	if !errors.As(err, &re) {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errClosed) {
			return fmt.Errorf("codexapp: inject ChatGPT token: %w", err)
		}
		return errors.New("codexapp: inject ChatGPT token: transport failure")
	}
	msg := strings.ToLower(re.Message)
	if re.Code == -32601 || strings.Contains(msg, "experimentalapi") || strings.Contains(msg, "unknown variant") || strings.Contains(msg, loginTypeTokens) {
		return fmt.Errorf("%w (rpc code %d)", errExternalAuthUnsupported, re.Code)
	}
	return fmt.Errorf("codexapp: App Server rejected the central ChatGPT token (rpc code %d)", re.Code)
}

// refresh answers one account/chatgptAuthTokens/refresh. Concurrent requests
// (several backend calls failing together) share a single provider call. The
// server's previousAccountId is only a hint: the bound account is what the
// provider is asked for and what the result must match.
func (a *tokenAuth) refresh(ctx context.Context) (ChatGPTToken, error) {
	a.mu.Lock()
	bound := a.account
	if !a.injected || bound == "" {
		a.mu.Unlock()
		return ChatGPTToken{}, errNotInjected
	}
	if f := a.inflight; f != nil {
		a.mu.Unlock()
		select {
		case <-f.done:
			return f.tok, f.err
		case <-ctx.Done():
			return ChatGPTToken{}, ctx.Err()
		}
	}
	f := &tokenRefresh{done: make(chan struct{})}
	a.inflight = f
	a.mu.Unlock()

	tok, err := a.fetch(ctx, true, bound)
	if err == nil && tok.AccountID != bound {
		tok, err = ChatGPTToken{}, errAccountChanged
	}
	f.tok, f.err = tok, err
	a.mu.Lock()
	a.inflight = nil
	a.mu.Unlock()
	close(f.done)
	return tok, err
}

// handleTokenRefresh serves the App Server's refresh request off the read loop,
// so a slow provider never stalls notifications or other responses.
func (s *Supervisor) handleTokenRefresh(id json.RawMessage, _ json.RawMessage) {
	s.mu.Lock()
	rpc, parent := s.rpc, s.runCtx
	s.mu.Unlock()
	if rpc == nil || parent == nil {
		return
	}
	if s.auth == nil {
		// Legacy launch: amux did not inject tokens, so it has none to refresh.
		_ = rpc.respondErr(id, -32601, "unsupported server request")
		return
	}
	rawID := append(json.RawMessage(nil), id...)
	go func() {
		// Bounded by the read loop too: a refresh still running when the
		// supervisor closes is abandoned rather than answered or logged late.
		ctx, cancel := context.WithTimeout(parent, refreshTimeout)
		defer cancel()
		tok, err := s.auth.refresh(ctx)
		if parent.Err() != nil {
			return
		}
		if err != nil {
			reason := "central ChatGPT auth refresh failed"
			switch {
			case errors.Is(err, errAccountChanged):
				reason = "central ChatGPT auth returned a different account"
			case errors.Is(err, errNotInjected):
				reason = "no central ChatGPT token was injected"
			case errors.Is(err, context.DeadlineExceeded):
				reason = "central ChatGPT auth refresh timed out"
			}
			s.emit(notice("error", "codex: "+reason+"; the request that needed it will fail"))
			_ = rpc.respondErr(rawID, -32000, reason)
			return
		}
		result := map[string]any{"accessToken": tok.AccessToken, "chatgptAccountId": tok.AccountID, "chatgptPlanType": nil}
		if tok.PlanType != "" {
			result["chatgptPlanType"] = tok.PlanType
		}
		_ = rpc.respond(rawID, result)
	}()
}
