package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	entered chan struct{} // signalled when a refresh starts waiting
}

func newStubProvider() *stubProvider { return &stubProvider{account: "acct-1"} }

func (p *stubProvider) CodexChatGPTToken(ctx context.Context, req TokenRequest) (ChatGPTToken, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	block, entered, account, err := p.block, p.entered, p.account, p.err
	p.mu.Unlock()
	if !req.Refresh {
		return ChatGPTToken{AccessToken: firstToken, AccountID: account, PlanType: "pro"}, nil
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

func TestManagerPassesProviderToConfig(t *testing.T) {
	m := NewManager(context.Background(), "")
	p := newStubProvider()
	m.SetChatGPTAuth(p)
	m.mu.Lock()
	got := m.chatgptAuth
	m.mu.Unlock()
	if got != TokenProvider(p) {
		t.Fatal("provider not retained")
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
