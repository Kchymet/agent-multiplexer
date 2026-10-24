package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
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
)

// tokenAuth is a supervisor's view of central auth: the provider plus the
// account this App Server was bound to, and the in-flight refresh shared by
// concurrent 401s.
type tokenAuth struct {
	provider  TokenProvider
	sessionID string

	mu       sync.Mutex
	account  string
	inflight *tokenRefresh
}

type tokenRefresh struct {
	done chan struct{}
	tok  ChatGPTToken
	err  error
}

func newTokenAuth(p TokenProvider, sessionID string) *tokenAuth {
	if p == nil {
		return nil
	}
	return &tokenAuth{provider: p, sessionID: sessionID}
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
// handshake, so a reconnect re-establishes auth before any thread RPC; the
// account bound by the first login is kept for the supervisor's lifetime.
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
	a.mu.Lock()
	a.account = tok.AccountID
	a.mu.Unlock()
	return nil
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
// (several backend calls failing together) share a single provider call.
func (a *tokenAuth) refresh(ctx context.Context, previous string) (ChatGPTToken, error) {
	a.mu.Lock()
	bound := a.account
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

	if previous == "" {
		previous = bound
	}
	tok, err := a.fetch(ctx, true, previous)
	if err == nil && bound != "" && tok.AccountID != bound {
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
func (s *Supervisor) handleTokenRefresh(id json.RawMessage, params json.RawMessage) {
	s.mu.Lock()
	rpc := s.rpc
	s.mu.Unlock()
	if rpc == nil {
		return
	}
	if s.auth == nil {
		// Legacy launch: amux did not inject tokens, so it has none to refresh.
		_ = rpc.respondErr(id, -32601, "unsupported server request")
		return
	}
	var p struct {
		PreviousAccountID string `json:"previousAccountId"`
	}
	_ = json.Unmarshal(params, &p)
	rawID := append(json.RawMessage(nil), id...)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		tok, err := s.auth.refresh(ctx, p.PreviousAccountID)
		if err != nil {
			reason := "central ChatGPT auth refresh failed"
			switch {
			case errors.Is(err, errAccountChanged):
				reason = "central ChatGPT auth returned a different account"
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
