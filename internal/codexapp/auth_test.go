package codexapp

import (
	"amux/internal/accounts"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	firstToken = "tok-first-SECRET"
	nextToken  = "tok-next-SECRET"
)

// stubProvider is a central token source: the first call yields firstToken,
// refreshes yield nextToken. Tests can make it block, fail, or switch account.
type stubProvider struct {
	mu      sync.Mutex
	reqs    []TokenRequest
	account string
	err     error
	block   chan struct{} // when non-nil, refreshes wait for it (or ctx)
	current string        // when set, the non-refresh token (another session rotated)
	entered chan struct{} // signalled when a refresh starts waiting
}

func newStubProvider() *stubProvider { return &stubProvider{account: "acct-1"} }

func (p *stubProvider) CodexChatGPTToken(ctx context.Context, req TokenRequest) (ChatGPTToken, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	block, entered, account, err, current := p.block, p.entered, p.account, p.err, p.current
	p.mu.Unlock()
	if !req.Refresh {
		if current == "" {
			current = firstToken
		}
		return ChatGPTToken{AccessToken: current, AccountID: account, PlanType: "pro"}, nil
	}
	if block != nil {
		if entered != nil {
			entered <- struct{}{}
		}
		select {
		case <-block:
		case <-ctx.Done():
			return ChatGPTToken{}, ctx.Err()
		}
	}
	if err != nil {
		return ChatGPTToken{}, err
	}
	return ChatGPTToken{AccessToken: nextToken, AccountID: account, PlanType: "pro"}, nil
}

func (p *stubProvider) requests() []TokenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TokenRequest(nil), p.reqs...)
}

func newAuthPair(t *testing.T, p TokenProvider) (*Supervisor, *fakeServer, *memConn) {
	t.Helper()
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	go fs.loop()
	sup := New(Config{
		SessionID:    "s-auth",
		Endpoint:     "unix:///tmp/x.sock",
		EventLogPath: filepath.Join(t.TempDir(), "events.jsonl"),
		ChatGPTAuth:  p,
	})
	return sup, fs, client
}

// assertNoSecrets checks every surface a token could leak through: the
// durable event log (what the web bridge replays) and any error text.
func assertNoSecrets(t *testing.T, sup *Supervisor, extra ...string) {
	t.Helper()
	data, err := os.ReadFile(sup.cfg.EventLogPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, text := range append(extra, string(data)) {
		if strings.Contains(text, "SECRET") {
			t.Fatalf("token leaked: %s", text)
		}
	}
}

func methodIndex(fs *fakeServer, method string) int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for i, c := range fs.calls {
		if c.Method == method {
			return i
		}
	}
	return -1
}

func refreshRequest(fs *fakeServer, id string) incoming {
	return fs.pushRequest(methodTokenRefresh, id, map[string]any{"reason": "unauthorized", "previousAccountId": "acct-1"})
}

func TestTokenInjectedBeforeThreadStart(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	login, ok := fs.sawCall("account/login/start")
	if !ok {
		t.Fatal("no account/login/start")
	}
	var params map[string]any
	_ = json.Unmarshal(login.Params, &params)
	if params["type"] != "chatgptAuthTokens" || params["accessToken"] != firstToken || params["chatgptAccountId"] != "acct-1" || params["chatgptPlanType"] != "pro" {
		t.Fatalf("login params = %v", params)
	}
	if li, ti, ii := methodIndex(fs, "account/login/start"), methodIndex(fs, "thread/start"), methodIndex(fs, "initialized"); !(ii < li && li < ti) {
		t.Fatalf("order initialized=%d login=%d thread/start=%d; login must precede every thread RPC", ii, li, ti)
	}
	if reqs := p.requests(); len(reqs) != 1 || reqs[0].Refresh || reqs[0].SessionID != "s-auth" || reqs[0].PreviousAccountID != "" {
		t.Fatalf("provider requests = %+v", reqs)
	}
	assertNoSecrets(t, sup)
}

func TestLegacyLaunchWithoutProvider(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	if _, ok := fs.sawCall("account/login/start"); ok {
		t.Fatal("legacy launch must not log in")
	}
	resp := refreshRequest(fs, "r-legacy")
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("legacy refresh response = %+v, want -32601", resp)
	}
}

func TestTokenRefreshAnswersServer(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	resp := refreshRequest(fs, "r1")
	if resp.Error != nil {
		t.Fatalf("refresh error: %+v", resp.Error)
	}
	var r map[string]any
	_ = json.Unmarshal(resp.Result, &r)
	if r["accessToken"] != nextToken || r["chatgptAccountId"] != "acct-1" || r["chatgptPlanType"] != "pro" {
		t.Fatalf("refresh result = %v", r)
	}
	reqs := p.requests()
	if last := reqs[len(reqs)-1]; !last.Refresh || last.PreviousAccountID != "acct-1" || last.SessionID != "s-auth" {
		t.Fatalf("refresh request = %+v", last)
	}
	assertNoSecrets(t, sup)
}

// Several backend calls can fail with 401 together; they must share one
// provider refresh, and a slow provider must not stall the read loop.
func TestConcurrentRefreshesShareOneProviderCall(t *testing.T) {
	p := newStubProvider()
	p.block = make(chan struct{})
	p.entered = make(chan struct{}, 4)
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	results := make(chan incoming, 3)
	for i := range 3 {
		go func() { results <- refreshRequest(fs, fmt.Sprintf("r%d", i)) }()
	}
	<-p.entered
	// The read loop is still serving while the refresh is in flight.
	fs.pushNotify("account/rateLimits/updated", map[string]any{})
	if !waitFor(func() bool { return len(fs.callsOf("account/login/start")) == 1 }, time.Second) {
		t.Fatal("login state changed unexpectedly")
	}
	time.Sleep(50 * time.Millisecond) // let the other requests join the in-flight refresh
	close(p.block)
	for range 3 {
		resp := <-results
		var r map[string]any
		_ = json.Unmarshal(resp.Result, &r)
		if resp.Error != nil || r["accessToken"] != nextToken {
			t.Fatalf("refresh response = %+v", resp)
		}
	}
	refreshes := 0
	for _, r := range p.requests() {
		if r.Refresh {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("provider refreshed %d times, want 1", refreshes)
	}
}

func TestRefreshFailureIsTokenFree(t *testing.T) {
	p := newStubProvider()
	p.err = errors.New("upstream said SECRET-detail")
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	resp := refreshRequest(fs, "r-fail")
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Fatalf("refresh response = %+v, want error", resp)
	}
	assertNoSecrets(t, sup, resp.Error.Message)
	if !strings.Contains(eventLog(t, sup), "central ChatGPT auth refresh failed") {
		t.Fatal("refresh failure not surfaced as a notice")
	}
}

func TestRefreshTimesOutBeforeServerDeadline(t *testing.T) {
	old := refreshTimeout
	refreshTimeout = 100 * time.Millisecond
	defer func() { refreshTimeout = old }()
	p := newStubProvider()
	p.block = make(chan struct{}) // never released
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	start := time.Now()
	resp := refreshRequest(fs, "r-slow")
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "timed out") {
		t.Fatalf("refresh response = %+v, want timeout error", resp)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout reply took %v", time.Since(start))
	}
	if old > 9*time.Second {
		t.Fatalf("refreshTimeout %v leaves no margin under the server's ~10s wait", old)
	}
}

func TestRefreshRefusesAccountSwitch(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	p.mu.Lock()
	p.account = "acct-other"
	p.mu.Unlock()
	resp := refreshRequest(fs, "r-switch")
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "different account") {
		t.Fatalf("refresh response = %+v, want account refusal", resp)
	}
	assertNoSecrets(t, sup, resp.Error.Message)
}

func TestUnsupportedServerFailsLaunchWithoutLeak(t *testing.T) {
	sup, fs, client := newAuthPair(t, newStubProvider())
	fs.rejectTokenLogin = true
	defer fs.close()
	defer sup.Close()
	err := sup.attach(context.Background(), client)
	if !errors.Is(err, errExternalAuthUnsupported) {
		t.Fatalf("attach = %v, want errExternalAuthUnsupported", err)
	}
	if _, ok := fs.sawCall("thread/start"); ok {
		t.Fatal("thread started without central auth")
	}
	assertNoSecrets(t, sup, err.Error())
}

func TestIncompleteProviderTokenFailsLaunch(t *testing.T) {
	p := providerFunc(func(context.Context, TokenRequest) (ChatGPTToken, error) {
		return ChatGPTToken{AccessToken: "x"}, nil
	})
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	if err := sup.attach(context.Background(), client); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("attach = %v, want incomplete credential", err)
	}
	if _, ok := fs.sawCall("account/login/start"); ok {
		t.Fatal("incomplete token was injected")
	}
}

// A reconnect (daemon restart or a dead transport) builds a new supervisor
// whose App Server has no tokens; it must log in again before resuming.
func TestReconnectReinjectsBeforeResume(t *testing.T) {
	p := newStubProvider()
	for _, resume := range []string{"", "thr_1"} {
		sup, fs, client := newAuthPair(t, p)
		sup.cfg.ResumeThreadID = resume
		attach(t, sup, client)
		li, ri := methodIndex(fs, "account/login/start"), methodIndex(fs, "thread/resume")
		if li < 0 || ri < 0 || li > ri {
			t.Fatalf("resume=%q: login=%d resume=%d", resume, li, ri)
		}
		_ = sup.Close()
		fs.close()
	}
	if n := len(p.requests()); n != 2 {
		t.Fatalf("provider asked %d times, want once per handshake", n)
	}
}

func TestTokenFormattingRedacts(t *testing.T) {
	tok := ChatGPTToken{AccessToken: firstToken, AccountID: "acct-1"}
	b, _ := json.Marshal(tok)
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), string(b)} {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("formatted token leaked: %s", s)
		}
	}
}

type providerFunc func(context.Context, TokenRequest) (ChatGPTToken, error)

func (f providerFunc) CodexChatGPTToken(ctx context.Context, r TokenRequest) (ChatGPTToken, error) {
	return f(ctx, r)
}

func eventLog(t *testing.T, sup *Supervisor) string {
	t.Helper()
	var data []byte
	waitFor(func() bool {
		data, _ = os.ReadFile(sup.cfg.EventLogPath)
		return len(data) > 0
	}, time.Second)
	return string(data)
}

func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// A refresh request that arrives before amux injected anything (a confused or
// hostile server) must not obtain a token.
func TestRefreshBeforeInjectionRefused(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	hold := fs.holdResponse("account/login/start")
	done := make(chan error, 1)
	go func() { done <- sup.attach(context.Background(), client) }()
	if !fs.awaitCall("account/login/start", time.Second) {
		t.Fatal("login never sent")
	}
	resp := refreshRequest(fs, "r-early")
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "no central ChatGPT token") {
		t.Fatalf("early refresh = %+v, want refusal", resp)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, r := range p.requests() {
		if r.Refresh {
			t.Fatal("provider refreshed for an early request")
		}
	}
}

// The server's previousAccountId is a hint; the provider is asked for the
// account amux bound, and a server naming another account cannot redirect it.
func TestRefreshUsesBoundAccountNotServerHint(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	resp := fs.pushRequest(methodTokenRefresh, "r-hint", map[string]any{"reason": "unauthorized", "previousAccountId": "acct-evil"})
	if resp.Error != nil {
		t.Fatalf("refresh error: %+v", resp.Error)
	}
	reqs := p.requests()
	if got := reqs[len(reqs)-1].PreviousAccountID; got != "acct-1" {
		t.Fatalf("provider asked for %q, want bound acct-1", got)
	}
}

// A refresh still running when the supervisor closes is abandoned: no late
// reply, and no late event reopening the closed log.
func TestRefreshAfterCloseIsDropped(t *testing.T) {
	p := newStubProvider()
	p.block = make(chan struct{})
	p.entered = make(chan struct{}, 1)
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	attach(t, sup, client)

	rawID, _ := json.Marshal("r-late")
	ch := make(chan incoming, 1)
	fs.mu.Lock()
	fs.respByID[string(rawID)] = ch
	fs.mu.Unlock()
	fs.write(map[string]any{"method": methodTokenRefresh, "id": "r-late", "params": map[string]any{"reason": "unauthorized"}})
	<-p.entered
	before := eventLog(t, sup)
	_ = sup.Close()
	close(p.block)
	select {
	case resp := <-ch:
		t.Fatalf("late refresh answered: %+v", resp)
	case <-time.After(100 * time.Millisecond):
	}
	if after, _ := os.ReadFile(sup.cfg.EventLogPath); string(after) != before {
		t.Fatalf("event log changed after Close:\n%s", after)
	}
}

// Within one process (a reconnect) the bound account is a constraint: a
// central login now pointing at another workspace fails instead of moving it.
func TestReconnectRefusesDifferentBoundAccount(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	sup.cfg.ResumeThreadID = "thr_1"
	sup.auth = newTokenAuth(p, "s-auth", "acct-old", accounts.Launch{})
	if err := sup.attach(context.Background(), client); !errors.Is(err, errAccountChanged) {
		t.Fatalf("attach = %v, want errAccountChanged", err)
	}
	if _, ok := fs.sawCall("account/login/start"); ok {
		t.Fatal("token for another account was injected")
	}
	if _, ok := fs.sawCall("thread/resume"); ok {
		t.Fatal("thread resumed under another account")
	}
}

func TestIdentityRecordsBoundAccount(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	sup, fs, client := newAuthPair(t, newStubProvider())
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	id := sup.Identity()
	if id.ChatGPTAccount != "acct-1" {
		t.Fatalf("identity account = %q", id.ChatGPTAccount)
	}
	if err := SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	// A later process of the same thread records the account as history and
	// is not constrained by it; another thread inherits nothing.
	if got := priorAccountsFor("s-auth", id.ThreadID); len(got) != 1 || got[0] != "acct-1" {
		t.Fatalf("priorAccountsFor = %q", got)
	}
	if got := priorAccountsFor("s-auth", "thr_other"); len(got) != 0 {
		t.Fatalf("another thread inherited the account: %q", got)
	}
	data, _ := os.ReadFile(identityPath("s-auth"))
	if strings.Contains(string(data), "SECRET") {
		t.Fatal("identity persisted a token")
	}
}

// TestManagerEnsureCentralAuthLifecycle drives the production wiring end to
// end: Manager.SetChatGPTAuth → Ensure (real child process, WebSocket dial) →
// injection before the thread → refresh over the wire → restart resumes the
// thread only under the persisted account.
func TestManagerEnsureCentralAuthLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	servers := make(chan *fakeServer, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		servers <- fs
		fs.loop()
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	ensure := func(m *Manager) (*Supervisor, error) {
		return m.Ensure(ctx, "central", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
	}

	p := newStubProvider()
	m := NewManager(ctx, "")
	m.SetChatGPTAuth(p)
	sup, err := ensure(m)
	if err != nil {
		t.Fatal(err)
	}
	fs := <-servers
	if li, ti := methodIndex(fs, "account/login/start"), methodIndex(fs, "thread/start"); li < 0 || li > ti {
		t.Fatalf("login=%d thread/start=%d", li, ti)
	}
	resp := refreshRequest(fs, "r-ws")
	var r map[string]any
	_ = json.Unmarshal(resp.Result, &r)
	if resp.Error != nil || r["accessToken"] != nextToken {
		t.Fatalf("refresh over WebSocket = %+v", resp)
	}
	threadID := sup.ThreadID()
	if id, ok := LoadIdentity("central"); !ok || id.ChatGPTAccount != "acct-1" {
		t.Fatalf("identity = %+v", id)
	}
	assertNoSecrets(t, sup)
	m.Shutdown()

	// Central login now points at another workspace: the resume is refused.
	other := newStubProvider()
	other.account = "acct-other"
	m2 := NewManager(ctx, "")
	m2.SetChatGPTAuth(other)
	if _, err := ensure(m2); !errors.Is(err, errAccountChanged) {
		t.Fatalf("Ensure with another account = %v, want errAccountChanged", err)
	}
	refused := <-servers
	if _, ok := refused.sawCall("thread/resume"); ok {
		t.Fatal("thread resumed under another account")
	}
	m2.Shutdown()

	// Same account: the restart logs in again, then resumes the same thread.
	m3 := NewManager(ctx, "")
	defer m3.Shutdown()
	m3.SetChatGPTAuth(p)
	sup3, err := ensure(m3)
	if err != nil {
		t.Fatal(err)
	}
	resumed := <-servers
	if li, ri := methodIndex(resumed, "account/login/start"), methodIndex(resumed, "thread/resume"); li < 0 || li > ri {
		t.Fatalf("restart login=%d resume=%d", li, ri)
	}
	if sup3.ThreadID() != threadID {
		t.Fatal("restart changed the thread")
	}
}

// Codex acknowledges account/login/start and only then finishes the login;
// a later account/login/completed success=false (e.g. workspace routing
// discovery failed) must fail the launch before any thread operation.
func TestAsyncLoginFailureBlocksThread(t *testing.T) {
	sup, fs, client := newAuthPair(t, newStubProvider())
	fs.loginFailure = "selected workspace missing from routing discovery"
	defer fs.close()
	defer sup.Close()
	err := sup.attach(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "selected workspace missing from routing discovery") {
		t.Fatalf("attach = %v, want the completion failure", err)
	}
	for _, m := range []string{"thread/start", "thread/resume"} {
		if _, ok := fs.sawCall(m); ok {
			t.Fatalf("%s sent after a failed login", m)
		}
	}
	assertNoSecrets(t, sup, err.Error())
	// A refresh after the failed login is refused: the server holds no usable login.
	if resp := refreshRequest(fs, "r-after-fail"); resp.Error == nil {
		t.Fatalf("refresh after failed login answered: %+v", resp)
	}
}

func TestMissingLoginCompletionTimesOut(t *testing.T) {
	old := loginCompletionTimeout
	loginCompletionTimeout = 100 * time.Millisecond
	defer func() { loginCompletionTimeout = old }()
	sup, fs, client := newAuthPair(t, newStubProvider())
	fs.skipLoginCompleted = true
	defer fs.close()
	defer sup.Close()
	err := sup.attach(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "no account/login/completed") {
		t.Fatalf("attach = %v, want completion timeout", err)
	}
	if _, ok := fs.sawCall("thread/start"); ok {
		t.Fatal("thread started without a completed login")
	}
}

// A completion error that echoes the token is withheld, not repeated.
func TestLoginCompletionErrorRedacted(t *testing.T) {
	sup, fs, client := newAuthPair(t, newStubProvider())
	fs.loginFailure = "bad token " + firstToken
	defer fs.close()
	defer sup.Close()
	err := sup.attach(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("attach = %v, want withheld reason", err)
	}
	// The completion itself must not reach the event stream verbatim either.
	if !waitFor(func() bool { return strings.Contains(eventLog(t, sup), "login") }, time.Second) {
		t.Fatal("login outcome not surfaced")
	}
	assertNoSecrets(t, sup, err.Error())
}

// A completion for another login (a browser login started by a native
// client carries a loginId) is not the token login's outcome.
func TestForeignLoginCompletionIgnored(t *testing.T) {
	a := newTokenAuth(newStubProvider(), "s", "", accounts.Launch{})
	ch := make(chan loginOutcome, 1)
	a.completed = ch
	if _, ok := a.loginCompleted(json.RawMessage(`{"loginId":"browser-1","success":true}`)); ok {
		t.Fatal("foreign completion consumed")
	}
	select {
	case out := <-ch:
		t.Fatalf("foreign completion delivered: %+v", out)
	default:
	}
}

// A 401 for a request still carrying the previous token can arrive after the
// refresh that replaced it finished. It gets the current token without a
// second forced rotation; a 401 after the debounce window forces again.
func TestStraggler401DoesNotRotateAgain(t *testing.T) {
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	forced := func() int {
		n := 0
		for _, r := range p.requests() {
			if r.Refresh {
				n++
			}
		}
		return n
	}
	for i, id := range []string{"r-first", "r-straggler"} {
		resp := refreshRequest(fs, id)
		if resp.Error != nil {
			t.Fatalf("refresh %d: %+v", i, resp.Error)
		}
	}
	if n := forced(); n != 1 {
		t.Fatalf("forced refreshes = %d, want 1 (straggler must not rotate)", n)
	}
	reqs := p.requests()
	if last := reqs[len(reqs)-1]; last.Refresh || last.PreviousAccountID != "acct-1" {
		t.Fatalf("straggler request = %+v, want current-token fetch for the bound account", last)
	}

	old := refreshDebounce
	refreshDebounce = 0
	defer func() { refreshDebounce = old }()
	if resp := refreshRequest(fs, "r-later"); resp.Error != nil {
		t.Fatalf("later refresh: %+v", resp.Error)
	}
	if n := forced(); n != 2 {
		t.Fatalf("forced refreshes = %d, want 2 after the debounce window", n)
	}
}

// The straggler window cannot hide a genuine rejection of the new token for
// longer than refreshDebounce: Codex's refresh request names no token
// (ChatgptAuthTokensRefreshParams carries only reason and previousAccountId)
// and retries each failing request once, so amux can only tell a straggler by
// time. Within the window it must still hand out the provider's newest token,
// stragglers must not extend the window, and the first 401 after it forces.
func TestStragglerWindowNeitherHidesNorExtends(t *testing.T) {
	old := refreshDebounce
	refreshDebounce = 300 * time.Millisecond
	defer func() { refreshDebounce = old }()
	p := newStubProvider()
	sup, fs, client := newAuthPair(t, p)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	token := func(resp incoming) string {
		t.Helper()
		if resp.Error != nil {
			t.Fatalf("refresh error: %+v", resp.Error)
		}
		var r map[string]any
		_ = json.Unmarshal(resp.Result, &r)
		s, _ := r["accessToken"].(string)
		return s
	}
	forced := func() int {
		n := 0
		for _, r := range p.requests() {
			if r.Refresh {
				n++
			}
		}
		return n
	}
	start := time.Now()
	if got := token(refreshRequest(fs, "r-rotate")); got != nextToken || forced() != 1 {
		t.Fatalf("rotation = %q forced=%d", got, forced())
	}
	// Another session rotated since: the straggler gets that newer token,
	// not the one this session last saw.
	p.mu.Lock()
	p.current = "tok-third-SECRET"
	p.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	if got := token(refreshRequest(fs, "r-straggler")); got != "tok-third-SECRET" || forced() != 1 {
		t.Fatalf("straggler = %q forced=%d, want provider's newest token without rotation", got, forced())
	}
	// 300ms after the ROTATION (not after the straggler) the window is over:
	// a 401 now is a rejection of the current token and forces a refresh.
	time.Sleep(time.Until(start.Add(refreshDebounce + 20*time.Millisecond)))
	if got := token(refreshRequest(fs, "r-true-failure")); got != nextToken || forced() != 2 {
		t.Fatalf("post-window refresh = %q forced=%d, want a forced rotation", got, forced())
	}
	if refreshDebounce > 30*time.Second || old > 30*time.Second {
		t.Fatal("straggler window must stay short")
	}
}
