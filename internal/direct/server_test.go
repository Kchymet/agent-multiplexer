package direct

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"

	"github.com/gorilla/websocket"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func nonce() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestHostAndOriginGateRunFirst(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead}, allSessions)

	r := httptest.NewRequest("GET", PathSession, nil)
	r.Host = "evil.example"
	r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest || apiCode(t, w) != CodeEndpointNotBound {
		t.Fatalf("foreign host: %d %s", w.Code, w.Body)
	}
	w = do(s, "GET", PathSession, tok.AccessToken, "", "Origin", "https://evil.example")
	if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign origin: %d %v", w.Code, w.Header())
	}
	if f.total() != 0 {
		t.Fatal("backend called for a refused host/origin")
	}
	w = do(s, "OPTIONS", PathSession, "", "", "Origin", testOrigin, "Access-Control-Request-Method", "GET",
		"Access-Control-Request-Private-Network", "true")
	h := w.Header()
	if w.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != testOrigin ||
		h.Get("Access-Control-Allow-Credentials") != "" || h.Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight: %d %v", w.Code, h)
	}
	w = do(s, "GET", PathSession, tok.AccessToken, "", "Origin", testOrigin)
	if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != testOrigin || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("allowed origin: %d %v", w.Code, w.Header())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
}

func TestHelloBindsNonceEndpointAndProtocol(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, newFake())
	req := HelloRequest{Protocol: Protocol, ClientNonce: nonce(), Endpoint: testEndpoint}
	body, _ := json.Marshal(req)
	w := do(s, "POST", PathHello, "", string(body))
	if w.Code != 200 {
		t.Fatalf("hello: %d %s", w.Code, w.Body)
	}
	var hr HelloResponse
	_ = json.Unmarshal(w.Body.Bytes(), &hr)
	if _, err := VerifyHello(hr.Proof, a.ServerID(), req, time.Now()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	other := req
	other.ClientNonce = nonce()
	if _, err := VerifyHello(hr.Proof, a.ServerID(), other, time.Now()); err == nil {
		t.Fatal("proof replayed against a fresh nonce")
	}
	if _, err := VerifyHello(hr.Proof, "someone-else", req, time.Now()); err == nil {
		t.Fatal("proof accepted for another server_id")
	}
	for name, bad := range map[string]HelloRequest{
		"endpoint": {Protocol: Protocol, ClientNonce: nonce(), Endpoint: "https://other.test:8443"},
		"protocol": {Protocol: "amux-direct/2", ClientNonce: nonce(), Endpoint: testEndpoint},
		"nonce":    {Protocol: Protocol, ClientNonce: "short", Endpoint: testEndpoint},
	} {
		b, _ := json.Marshal(bad)
		if w := do(s, "POST", PathHello, "", string(b)); w.Code == 200 {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTokenEndpointFormOnlyAndPerSourceLimit(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, newFake())
	good := enroll(t, a, []string{PermRead}, allSessions)
	send := func(src, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", PathToken+query, strings.NewReader(body))
		r.Host, r.RemoteAddr = testHost, src+":5000"
		r.Header.Set("Content-Type", formCT)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if w := send("10.0.0.9", "?code=x", form(url.Values{"grant_type": {GrantTypeEnrollmentCode}})); w.Code != 400 {
		t.Fatalf("secret in URL accepted: %d", w.Code)
	}
	bad := form(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {"amxr_nope"}})
	for range maxFailures {
		if w := send("10.0.0.9", "", bad); w.Code != 400 {
			t.Fatalf("bad token: %d", w.Code)
		}
	}
	if w := send("10.0.0.9", "", bad); w.Code != http.StatusTooManyRequests {
		t.Fatalf("abusive source not limited: %d", w.Code)
	}
	// Another enrolled client's renewal is unaffected by that peer.
	w := send("10.0.0.7", "", form(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {good.RefreshToken}}))
	if w.Code != 200 {
		t.Fatalf("legitimate refresh blocked by another source: %d %s", w.Code, w.Body)
	}
	var tr TokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil || tr.RefreshToken == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("token response: %v %s", err, w.Body)
	}
}

func TestDeniedOperationsMakeNoBackendCall(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	reader := enroll(t, a, []string{PermRead}, allSessions)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	cases := []struct {
		name, token, path, body, code string
	}{
		{"no token", "", PathSession, "", CodeAuthInvalid},
		{"unknown token", "amxa_unknown", PathSession + "/a1/history", "", CodeAuthInvalid},
		{"refresh as access", reader.RefreshToken, PathSession, "", CodeAuthInvalid},
		{"read-only prompt", reader.AccessToken, PathSession + "/a1/actions", actionBody(VerbPrompt, "hi", "k1"), CodeDenied},
		{"unsupported verb", steer.AccessToken, PathSession + "/a1/actions", actionBody("interject", "hi", "k2"), CodeUnsupportedVerb},
		{"extra field", steer.AccessToken, PathSession + "/a1/actions", `{"verb":"prompt","fields":{"text":"x","reason":"y"},"idem_key":"k3"}`, CodeBadRequest},
		{"stop with fields", steer.AccessToken, PathSession + "/a1/actions", actionBody(VerbStop, "x", "k4"), CodeBadRequest},
		{"empty prompt", steer.AccessToken, PathSession + "/a1/actions", actionBody(VerbPrompt, "  ", "k5"), CodeBadRequest},
		{"no idem key", steer.AccessToken, PathSession + "/a1/actions", actionBody(VerbPrompt, "hi", ""), CodeBadRequest},
		{"unknown action field", steer.AccessToken, PathSession + "/a1/actions", `{"verb":"prompt","fields":{"text":"x"},"idem_key":"k6","raw":"core"}`, CodeBadRequest},
	}
	for _, c := range cases {
		method := "GET"
		if c.body != "" {
			method = "POST"
		}
		w := do(s, method, c.path, c.token, c.body)
		if got := apiCode(t, w); got != c.code {
			t.Errorf("%s: code %q (%d %s)", c.name, got, w.Code, w.Body)
		}
	}
	if f.total() != 0 {
		t.Fatalf("denied operations reached the backend: snapshots=%d privileged=%d", f.snapshots.Load(), f.privileged())
	}
}

func TestScopeIsJudgedFromFreshState(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})

	w := do(s, "GET", PathSession, tok.AccessToken, "")
	var sr SessionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &sr)
	var ids []string
	for _, row := range sr.Sessions {
		ids = append(ids, row.ID)
	}
	if strings.Join(ids, ",") != "wg1,a1" {
		t.Fatalf("workgroup inventory: %v", ids)
	}
	for _, leak := range []string{"secret", "4242", "cwd", "pid", "claude:private", "account", "can_attach"} {
		if strings.Contains(strings.ToLower(w.Body.String()), leak) {
			t.Fatalf("inventory leaks %q: %s", leak, w.Body)
		}
	}

	before := f.privileged()
	for _, sid := range []string{"d1", "b1", "nope"} {
		if w := do(s, "GET", PathSession+"/"+sid+"/history", tok.AccessToken, ""); w.Code != 404 {
			t.Errorf("out-of-scope history %s: %d", sid, w.Code)
		}
		if w := do(s, "POST", PathSession+"/"+sid+"/actions", tok.AccessToken, actionBody(VerbPrompt, "hi", "x-"+sid)); w.Code != 404 {
			t.Errorf("out-of-scope prompt %s: %d", sid, w.Code)
		}
	}
	if f.privileged() != before {
		t.Fatal("out-of-scope targets reached Apply/History/Stream")
	}

	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "hello", "k1")); w.Code != 200 {
		t.Fatalf("in-scope prompt: %d %s", w.Code, w.Body)
	}
	got := <-f.applied
	if got.Action != core.ActionSteer || got.ID != "a1" || got.Fields[core.SteerVerb] != core.SteerPrompt || got.Fields[core.SteerText] != "hello" || len(got.Fields) != 2 {
		t.Fatalf("applied %+v", got)
	}
	// The member moves to another workgroup: the next call sees it immediately.
	rows := testRows()
	rows[1].RootID = "wg2"
	f.setRows(rows...)
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbStop, "", "k2")); w.Code != 404 {
		t.Fatalf("moved member still steerable: %d", w.Code)
	}
}

func TestArchivedTargetsAndParentsAreNotSteerable(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	for _, sid := range []string{"wg2", "b1"} { // b1 is live but its root is archived
		w := do(s, "POST", PathSession+"/"+sid+"/actions", tok.AccessToken, actionBody(VerbPrompt, "hi", "k-"+sid))
		if w.Code != http.StatusConflict || apiCode(t, w) != CodeArchived {
			t.Errorf("%s: %d %s", sid, w.Code, w.Body)
		}
	}
	if f.applies.Load() != 0 {
		t.Fatal("archived target reached Apply")
	}
	w := do(s, "GET", PathSession, tok.AccessToken, "")
	var sr SessionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &sr)
	for _, row := range sr.Sessions {
		if (row.ID == "b1" || row.ID == "wg2") && (row.Caps.Prompt || row.Caps.Stop) {
			t.Errorf("%s advertises steering while archived", row.ID)
		}
	}
	reader := enroll(t, a, []string{PermRead}, allSessions)
	_ = json.Unmarshal(do(s, "GET", PathSession, reader.AccessToken, "").Body.Bytes(), &sr)
	for _, row := range sr.Sessions {
		if row.Caps.Prompt || row.Caps.Stop {
			t.Errorf("read-only grant sees steer caps on %s", row.ID)
		}
	}
}

func TestIdempotentActions(t *testing.T) {
	root := t.TempDir()
	f := newFake(testRows()...)
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	body := actionBody(VerbPrompt, "once", "same")
	for range 2 {
		if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, body); w.Code != 200 {
			t.Fatalf("prompt: %d %s", w.Code, w.Body)
		}
	}
	if f.applies.Load() != 1 {
		t.Fatalf("replay executed again: %d", f.applies.Load())
	}
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "different", "same")); apiCode(t, w) != CodeIdemConflict {
		t.Fatalf("conflict: %d %s", w.Code, w.Body)
	}
	// An action admitted by a process that died before recording its outcome is
	// ambiguous after restart and never runs again.
	l, err := a.Admit(context.Background(), tok.AccessToken, PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	ambiguousBody := actionBody(VerbStop, "", "crash")
	var req ActionRequest
	_ = json.Unmarshal([]byte(ambiguousBody), &req)
	if out, _, err := a.BeginAction(l, "crash", actionHash("a1", req), false); err != nil || out != idemProceed {
		t.Fatal(out, err)
	}
	l.Done()
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	fresh, err := b.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	s2 := newTestServer(t, b, f)
	applied := f.applies.Load()
	if w := do(s2, "POST", PathSession+"/a1/actions", fresh.AccessToken, ambiguousBody); apiCode(t, w) != CodeIdemAmbiguous {
		t.Fatalf("ambiguous: %d %s", w.Code, w.Body)
	}
	if f.applies.Load() != applied {
		t.Fatal("ambiguous action executed again")
	}
}

func TestStalledBackendIsCancelledAtAccessExpiry(t *testing.T) {
	f := newFake(testRows()...)
	f.blockSnapshot.Store(true)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- do(s, "GET", PathSession, tok.AccessToken, "") }()
	select {
	case w := <-done:
		if w.Code != 401 || apiCode(t, w) != CodeAuthExpired {
			t.Fatalf("stalled call: %d %s", w.Code, w.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled daemon held the request past access expiry")
	}
}

// sse reads SSE events from a live stream.
func sse(t *testing.T, resp *http.Response) <-chan [2]string {
	out := make(chan [2]string, 16)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return out
}

func openStream(t *testing.T, ts *httptest.Server, token, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Host = testHost
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	return resp
}

func nextEvent(t *testing.T, ch <-chan [2]string) [2]string {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return [2]string{}
}

func TestStreamRevocationIsTerminalAndBarriered(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	ts := httptest.NewServer(newTestServer(t, a, f))
	t.Cleanup(ts.Close) // registered first, so it runs after every stream body closes
	tok := enroll(t, a, []string{PermRead}, allSessions)
	other := enroll(t, a, []string{PermRead}, allSessions)
	events := sse(t, openStream(t, ts, tok.AccessToken, PathSession+"/a1/stream?after=3"))
	otherEvents := sse(t, openStream(t, ts, other.AccessToken, PathSession+"/wg1/stream"))
	<-f.streamStarted
	<-f.streamStarted
	f.feed("a1") <- hp.RuntimeEventBatch{Seq: 4, Runtime: "claude", Events: []hp.RuntimeEvent{{Type: hp.TypeText}}}
	if e := nextEvent(t, events); e[0] != "events" || !strings.Contains(e[1], `"seq":4`) {
		t.Fatalf("first event %v", e)
	}
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	// By the time Revoke returned, the stream had emitted its terminal event.
	if e := nextEvent(t, events); e[0] != "auth" || !strings.Contains(e[1], CodeAuthRevoked) {
		t.Fatalf("terminal event %v", e)
	}
	if _, ok := <-events; ok {
		t.Fatal("stream continued after auth.revoked")
	}
	// The other client's stream is unaffected: it still delivers data, not a
	// terminal auth event.
	f.feed("wg1") <- hp.RuntimeEventBatch{Seq: 9, Events: []hp.RuntimeEvent{{Type: hp.TypeText}}}
	if e := nextEvent(t, otherEvents); e[0] != "events" || !strings.Contains(e[1], `"seq":9`) {
		t.Fatalf("other stream: %v", e)
	}
}

func TestStreamExpiresDistinctlyEvenWhenIdle(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	ts := httptest.NewServer(newTestServer(t, a, f))
	t.Cleanup(ts.Close) // registered first, so it runs after every stream body closes
	tok := enroll(t, a, []string{PermRead}, allSessions)
	events := sse(t, openStream(t, ts, tok.AccessToken, PathSession+"/a1/stream"))
	e := nextEvent(t, events)
	if e[0] != "auth" || !strings.Contains(e[1], CodeAuthExpired) {
		t.Fatalf("idle expiry: %v", e)
	}
}

func dialWatch(t *testing.T, ts *httptest.Server, origin string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + PathWatch
	h := http.Header{"Host": {testHost}}
	if origin != "" {
		h.Set("Origin", origin)
	}
	d := websocket.Dialer{}
	conn, resp, err := d.Dial(u, h)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial watch: %v (%d)", err, status)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func closeCode(t *testing.T, conn *websocket.Conn) (int, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if ce, ok := err.(*websocket.CloseError); ok {
			return ce.Code, ce.Text
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

func TestWatchAuthenticatesFirstAndClosesOnRevokeAndExpiry(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: 2 * time.Second}, nil)
	srv := newTestServer(t, a, f)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = testHost
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	// Bad first frame: nothing sent, then 4401.
	conn := dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(map[string]string{"type": "auth", "access_token": "amxa_nope"})
	if code, reason := closeCode(t, conn); code != CloseAuthInvalid || reason != CodeAuthInvalid {
		t.Fatalf("bad token close: %d %s", code, reason)
	}
	if f.snapshots.Load() != 0 {
		t.Fatal("inventory read before authentication")
	}
	// Oversized first frame.
	conn = dialWatch(t, ts, testOrigin)
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth","access_token":"`+strings.Repeat("a", MaxWatchAuthSize)+`"}`))
	if code, _ := closeCode(t, conn); code == 0 {
		t.Fatal("oversized auth frame accepted")
	}

	tok := enroll(t, a, []string{PermRead}, allSessions)
	conn = dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	var welcome WatchWelcome
	if err := conn.ReadJSON(&welcome); err != nil || welcome.Type != "welcome" || welcome.ServerID != a.ServerID() {
		t.Fatalf("welcome: %v %+v", err, welcome)
	}
	var snap WatchSnapshot
	if err := conn.ReadJSON(&snap); err != nil || snap.Type != "snapshot" || len(snap.Sessions) != 5 {
		t.Fatalf("snapshot: %v %+v", err, snap)
	}
	go func() { time.Sleep(100 * time.Millisecond); _ = a.Revoke(tok.ClientID, "test") }()
	if code, reason := closeCode(t, conn); code != CloseAuthRevoked || reason != CodeAuthRevoked {
		t.Fatalf("revoke close: %d %s", code, reason)
	}

	tok2 := enroll(t, a, []string{PermRead}, allSessions)
	conn = dialWatch(t, ts, "") // a non-browser client sends no Origin
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok2.AccessToken})
	if code, reason := closeCode(t, conn); code != CloseAuthExpired || reason != CodeAuthExpired {
		t.Fatalf("expiry close: %d %s", code, reason)
	}
}

func TestCollidingSessionIDsAcrossIndependentServers(t *testing.T) {
	fa, fb := newFake(testRows()...), newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	b := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	sa, sb := newTestServer(t, a, fa), newTestServer(t, b, fb)
	ta := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	tb := enroll(t, b, []string{PermRead, PermSteer}, allSessions)
	if a.ServerID() == b.ServerID() {
		t.Fatal("independent servers share an identity")
	}
	// A credential from one server is meaningless on the other, even for the
	// same session id.
	if w := do(sb, "POST", PathSession+"/a1/actions", ta.AccessToken, actionBody(VerbPrompt, "x", "k")); apiCode(t, w) != CodeAuthInvalid {
		t.Fatalf("cross-server token: %d %s", w.Code, w.Body)
	}
	if fb.total() != 0 {
		t.Fatal("cross-server token reached the other backend")
	}
	if w := do(sa, "GET", PathSession, ta.AccessToken, ""); w.Code != 200 {
		t.Fatalf("server A: %d", w.Code)
	}
	if err := a.Revoke(ta.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if w := do(sb, "POST", PathSession+"/a1/actions", tb.AccessToken, actionBody(VerbPrompt, "x", "k")); w.Code != 200 {
		t.Fatalf("server B after revoking on A: %d %s", w.Code, w.Body)
	}
	if fa.applies.Load() != 0 || fb.applies.Load() != 1 {
		t.Fatalf("applies a=%d b=%d", fa.applies.Load(), fb.applies.Load())
	}
}

func TestRevokeEndpointSignsOutTheWholeGrant(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, newFake(testRows()...))
	tok := enroll(t, a, []string{PermRead}, allSessions)
	if w := do(s, "POST", PathRevoke, "", form(url.Values{"token": {tok.AccessToken}}), "Content-Type", formCT); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", PathSession, tok.AccessToken, ""); apiCode(t, w) != CodeAuthRevoked {
		t.Fatalf("after sign-out: %s", w.Body)
	}
	if w := do(s, "POST", PathRevoke, "", form(url.Values{"token": {"amxr_unknown"}}), "Content-Type", formCT); w.Code != 200 {
		t.Fatalf("unknown token revoke must succeed (RFC 7009): %d", w.Code)
	}
}

func TestRevokeBarrierAdmitsNoNewPrivilegedCall(t *testing.T) {
	f := newFake(testRows()...)
	f.blockApply.Store(true)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	victim := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	other := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	inflight := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		inflight <- do(s, "POST", PathSession+"/a1/actions", victim.AccessToken, actionBody(VerbPrompt, "slow", "k1"))
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.applies.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("action never reached Apply")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := a.Revoke(victim.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	// The barrier: the in-flight handler finished before Revoke returned.
	select {
	case w := <-inflight:
		if apiCode(t, w) != CodeAuthRevoked {
			t.Fatalf("in-flight action: %d %s", w.Code, w.Body)
		}
	default:
		t.Fatal("Revoke returned while the admitted action was still running")
	}
	f.blockApply.Store(false)
	applied := f.applies.Load()
	snaps := f.snapshots.Load()
	for i := range 5 {
		w := do(s, "POST", PathSession+"/a1/actions", victim.AccessToken, actionBody(VerbStop, "", "after"+string(rune('a'+i))))
		if apiCode(t, w) != CodeAuthRevoked {
			t.Fatalf("after revoke: %d %s", w.Code, w.Body)
		}
	}
	if f.applies.Load() != applied || f.snapshots.Load() != snaps {
		t.Fatal("a revoked client reached the backend after the barrier")
	}
	if w := do(s, "POST", PathSession+"/a1/actions", other.AccessToken, actionBody(VerbStop, "", "ok")); w.Code != 200 {
		t.Fatalf("other client: %d %s", w.Code, w.Body)
	}
}

func TestConsoleIsNeverExposed(t *testing.T) {
	rows := append(testRows(), core.Session{ID: ConsoleID, Title: "console", Role: hp.RoleConsole,
		Caps: &core.SessionCaps{Prompt: true, Cancel: true}, Runtime: "claude"})
	f := newFake(rows...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	if w := do(s, "GET", PathSession, tok.AccessToken, ""); strings.Contains(w.Body.String(), `"console"`) {
		t.Fatalf("console in all_sessions inventory: %s", w.Body)
	}
	before := f.privileged()
	for _, req := range [][3]string{
		{"GET", PathSession + "/console/history", ""},
		{"GET", PathSession + "/console/stream", ""},
		{"POST", PathSession + "/console/actions", actionBody(VerbPrompt, "rm -rf", "c1")},
	} {
		if w := do(s, req[0], req[1], tok.AccessToken, req[2]); w.Code != 404 {
			t.Errorf("%s %s: %d", req[0], req[1], w.Code)
		}
	}
	if f.privileged() != before {
		t.Fatal("console request reached the backend")
	}
	if _, _, err := a.NewCode("x", []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{ConsoleID}}, 0); err == nil {
		t.Fatal("--workgroup console accepted")
	}
}

func TestActionOutcomeClassification(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		status   int
		code     string
		retryOut string // code on retry with the same idem_key
	}{
		{"daemon refusal", &daemon.RejectedError{Msg: "no such agent"}, 422, CodeRejected, CodeRejected},
		{"never reached", hostbackend.ErrUnavailable, 503, CodeUnavailable, CodeUnavailable},
		{"transport failure after dispatch", errors.New("read: connection reset"), 409, CodeIdemAmbiguous, CodeIdemAmbiguous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(testRows()...)
			f.applyErr = c.err
			a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
			s := newTestServer(t, a, f)
			tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
			w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "hi", "k"))
			if w.Code != c.status || apiCode(t, w) != c.code {
				t.Fatalf("first: %d %s", w.Code, w.Body)
			}
			if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "hi", "k")); apiCode(t, w) != c.retryOut {
				t.Fatalf("retry: %d %s", w.Code, w.Body)
			}
			if f.applies.Load() != 1 {
				t.Fatalf("executed %d times", f.applies.Load())
			}
		})
	}
}

func TestAdmittedActionSurvivesExpiryAndDisconnectButNotRevocation(t *testing.T) {
	f := newFake(testRows()...)
	f.blockApply.Store(true)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	ctx, disconnect := context.WithCancel(context.Background())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", PathSession+"/a1/actions", strings.NewReader(actionBody(VerbPrompt, "long", "k"))).WithContext(ctx)
		r.Host = testHost
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		done <- w
	}()
	for f.applies.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	disconnect()
	time.Sleep(1500 * time.Millisecond) // the access token has expired meanwhile
	select {
	case w := <-done:
		t.Fatalf("admitted action abandoned on disconnect/expiry: %d %s", w.Code, w.Body)
	default:
	}
	// A retry with the same key while it runs reports in progress, not a rerun.
	fresh, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(s, "POST", PathSession+"/a1/actions", fresh.AccessToken, actionBody(VerbPrompt, "long", "k")); apiCode(t, w) != CodeIdemInProgress {
		t.Fatalf("retry while running: %d %s", w.Code, w.Body)
	}
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if apiCode(t, w) != CodeAuthRevoked {
		t.Fatalf("revoked action: %d %s", w.Code, w.Body)
	}
	if f.applies.Load() != 1 {
		t.Fatal("re-executed")
	}
	g := a.state.Grants[tok.ClientID]
	if g.Idem["k"].State != idemUnknown {
		t.Fatalf("revoked mid-call action recorded as %q", g.Idem["k"].State)
	}
}

func TestExpiryBeforeExecutionDenies(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	time.Sleep(1100 * time.Millisecond)
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "late", "k")); apiCode(t, w) != CodeAuthExpired {
		t.Fatalf("expired: %d %s", w.Code, w.Body)
	}
	if f.total() != 0 {
		t.Fatal("expired token reached the backend")
	}
}

func TestUnresolvedActionsAreKeptAndCapacityFailsClosed(t *testing.T) {
	old := maxIdemPerGrant
	maxIdemPerGrant = 2
	t.Cleanup(func() { maxIdemPerGrant = old })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	f := newFake(testRows()...)
	f.applyErr = errors.New("connection reset")
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	for _, k := range []string{"k1", "k2"} {
		if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbStop, "", k)); apiCode(t, w) != CodeIdemAmbiguous {
			t.Fatalf("%s: %s", k, w.Body)
		}
	}
	c.add(48 * time.Hour) // far beyond the retention of completed records
	fresh, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(s, "POST", PathSession+"/a1/actions", fresh.AccessToken, actionBody(VerbStop, "", "k1")); apiCode(t, w) != CodeIdemAmbiguous {
		t.Fatalf("old ambiguous key re-executable: %s", w.Body)
	}
	if w := do(s, "POST", PathSession+"/a1/actions", fresh.AccessToken, actionBody(VerbStop, "", "k3")); w.Code != http.StatusTooManyRequests {
		t.Fatalf("capacity: %d %s", w.Code, w.Body)
	}
	if f.applies.Load() != 2 {
		t.Fatalf("applies %d", f.applies.Load())
	}
}

// A poll that races token expiry must still close 4002, not drop the socket.
func TestWatchExpiryDuringPollStillSendsCloseCode(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	srv, err := NewServer(Config{Authority: a, Backend: f, Endpoints: []string{testEndpoint},
		WatchPoll: time.Millisecond, Revalidate: time.Hour, Keepalive: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { r.Host = testHost; srv.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	for range 5 {
		tok := enroll(t, a, []string{PermRead}, allSessions)
		f.blockSnapshot.Store(false)
		conn := dialWatch(t, ts, "")
		_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
		go func() { time.Sleep(300 * time.Millisecond); f.blockSnapshot.Store(true) }() // a poll is in flight at expiry
		if code, reason := closeCode(t, conn); code != CloseAuthExpired || reason != CodeAuthExpired {
			t.Fatalf("close: %d %s", code, reason)
		}
	}
}
