package direct

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestClassifyTLSError(t *testing.T) {
	for msg, want := range map[string]string{
		"remote error: tls: bad certificate":                       FailTLSMismatch,
		"remote error: tls: unknown certificate authority":         FailTLSMismatch,
		"tls: client offered only unsupported versions: [301]":     FailTLSMismatch,
		"tls: no cipher suite supported by both client and server": FailTLSMismatch,
		"client sent an HTTP request to an HTTPS server":           FailProtocol,
		"tls: first record does not look like a TLS handshake":     FailProtocol,
		"read tcp 127.0.0.1:8443->127.0.0.1:5555: i/o timeout":     FailTimeout,
		"EOF": FailClientClosed,
		"read tcp 1.2.3.4:8443->5.6.7.8:9: connection reset by peer": FailClientClosed,
		"something new": FailOther,
	} {
		if got := classifyTLSError(msg); got != want {
			t.Errorf("%q: %s, want %s", msg, got, want)
		}
	}
}

func TestClassifyAnswer(t *testing.T) {
	cases := []struct {
		route, code string
		status      int
		want        string
	}{
		{"sessions", CodeAuthInvalid, 401, FailAuthInvalid},
		{"sessions", CodeAuthExpired, 401, FailAuthExpired},
		{"sessions", CodeGrantChanged, 401, FailAuthExpired},
		{"sessions", CodeAuthRevoked, 401, FailAuthRevoked},
		{"unrouted", CodeEndpointNotBound, 421, FailConfigMismatch},
		{"unrouted", CodeOriginNotAllowed, 403, FailConfigMismatch},
		{"sessions", CodeUnavailable, 503, FailServerUnavailable},
		{"token", OAuthInvalidGrant, 400, FailAuthRevoked},
		{"claim", OAuthInvalidGrant, 400, FailAuthInvalid},
		{"token", OAuthTemporarilyUnavail, 429, FailRateLimited},
		{"token", OAuthTemporarilyUnavail, 503, FailServerUnavailable},
		{"hello", CodeProtocol, 400, FailProtocol},
		{"hello", CodeBadRequest, 400, FailProtocol},
		// Application answers are not connection failures.
		{"sessions/{sid}/actions", CodeDenied, 403, ""},
		{"sessions/{sid}/history", CodeNotFound, 404, ""},
		{"sessions/{sid}/actions", CodeIdemConflict, 422, ""},
		{"sessions", "", 200, ""},
	}
	for _, c := range cases {
		if got := classifyAnswer(c.route, c.status, c.code); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestErrorLogKeepsTLSErrorsForTheConnection(t *testing.T) {
	var logged []string
	tel := newConnTel(time.Now, func(f string, a ...any) { logged = append(logged, f) })
	_, _ = errorLog{tel}.Write([]byte("http: TLS handshake error from [::1]:5555: remote error: tls: bad certificate\nhttp: other thing\n"))
	if tel.tlsErrs["[::1]:5555"] != "remote error: tls: bad certificate" {
		t.Fatalf("pending: %v", tel.tlsErrs)
	}
	if len(logged) != 1 {
		t.Fatalf("other lines must still be logged: %v", logged)
	}
}

func TestListenCheckRecordsAddressChanges(t *testing.T) {
	var lines []string
	var mu sync.Mutex
	tel := newConnTel(time.Now, func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(f, a...))
	})
	have := []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.20")}}
	addrs := func() ([]net.Addr, error) { return have, nil }
	ip := net.ParseIP("192.168.1.20")
	tel.listenCheck("192.168.1.20:8443", addrAssigned(ip, addrs))
	if st := tel.snapshot(); !st.Listen.Present || st.Listen.Changes != 0 || len(st.Recent) != 0 {
		t.Fatalf("first check: %+v", st)
	}
	have = []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.1.31")}} // DHCP moved the host
	tel.listenCheck("192.168.1.20:8443", addrAssigned(ip, addrs))
	st := tel.snapshot()
	if st.Listen.Present || st.Listen.Changes != 1 || st.Failures[FailNetworkChanged] != 1 {
		t.Fatalf("after the address left: %+v", st)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "reason=network_changed") {
		t.Fatalf("log: %v", lines)
	}
	if !addrAssigned(net.ParseIP("0.0.0.0"), addrs) || !addrAssigned(ip, func() ([]net.Addr, error) { return nil, errors.New("x") }) {
		t.Fatal("unspecified bind or a lookup error must count as present")
	}
}

// Every refused request is recorded with the phase it reached and a reason
// from the taxonomy; plain successes are only counted.
func TestRequestAttemptsAreClassified(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead}, allSessions)

	if w := do(s, "GET", PathSession, tok.AccessToken, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	do(s, "GET", PathSession, "amxa_unknown", "")
	r := httptest.NewRequest("GET", PathSession, nil)
	r.Host = "other.example:8443"
	s.ServeHTTP(httptest.NewRecorder(), r)
	do(s, "POST", PathToken, "", form(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {"amxr_nope"}}), "Content-Type", formCT)

	st := s.ConnStats()
	if st.Requests != 4 || st.Authenticated != 1 {
		t.Fatalf("counters: %+v", st)
	}
	if st.Failures[FailAuthInvalid] != 1 || st.Failures[FailConfigMismatch] != 1 || st.Failures[FailAuthRevoked] != 1 {
		t.Fatalf("failures: %+v", st.Failures)
	}
	if st.FailuresByPhase[PhaseHTTP] != 2 || st.FailuresByPhase[PhaseTLS] != 1 {
		t.Fatalf("by phase: %+v", st.FailuresByPhase)
	}
	if len(st.Recent) != 3 {
		t.Fatalf("recent: %+v", st.Recent)
	}
	refresh := st.Recent[2]
	if refresh.Route != "token" || refresh.Detail != "refresh unknown" || refresh.Code != OAuthInvalidGrant {
		t.Fatalf("refresh attempt: %+v", refresh)
	}
	if st.Refresh.Unknown != 1 {
		t.Fatalf("refresh stats: %+v", st.Refresh)
	}
}

func watchServer(t *testing.T, s *Server) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = testHost
		s.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// A watch peer that stops answering (its network went away without a FIN)
// is closed by the server within three ping intervals and recorded as a
// timeout, instead of holding the socket and its lease until token expiry.
func TestWatchClosesADeadPeer(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s, err := NewServer(Config{Authority: a, Backend: f, Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
		WatchPoll: 20 * time.Millisecond, WatchPing: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := watchServer(t, s)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	conn := dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	// Never read again: gorilla answers pings only while reading, so this
	// client is as silent as one whose network vanished.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.ConnStats(); st.StreamEnds[FailTimeout] == 1 {
			if st.StreamsActive != 0 || st.StreamsOpened != 1 {
				t.Fatalf("stream counters: %+v", st)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dead peer not closed: %+v", s.ConnStats())
}

// A client that asks for heartbeats gets a frame at least every heartbeat
// interval even when the inventory does not change; one that does not ask
// sees exactly the frames it always did.
func TestWatchHeartbeatOptIn(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s, err := NewServer(Config{Authority: a, Backend: f, Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
		WatchPoll: 10 * time.Millisecond, WatchHeartbeat: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := watchServer(t, s)
	tok := enroll(t, a, []string{PermRead}, allSessions)

	conn := dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken, Heartbeat: true})
	var welcome WatchWelcome
	if err := conn.ReadJSON(&welcome); err != nil || welcome.Heartbeat != 50 {
		t.Fatalf("welcome: %v %+v", err, welcome)
	}
	var snap WatchSnapshot
	if err := conn.ReadJSON(&snap); err != nil || snap.Type != "snapshot" {
		t.Fatalf("snapshot: %v %+v", err, snap)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hb map[string]any
	if err := conn.ReadJSON(&hb); err != nil || hb["type"] != "heartbeat" {
		t.Fatalf("heartbeat: %v %v", err, hb)
	}

	plain := dialWatch(t, ts, testOrigin)
	_ = plain.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	var raw map[string]any
	if err := plain.ReadJSON(&raw); err != nil || raw["type"] != "welcome" {
		t.Fatalf("welcome: %v %v", err, raw)
	}
	if _, ok := raw["heartbeat_ms"]; ok {
		t.Fatal("heartbeat_ms announced to a client that did not ask")
	}
	_ = plain.ReadJSON(&raw) // the snapshot
	_ = plain.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if err := plain.ReadJSON(&raw); err == nil {
		t.Fatalf("unrequested frame: %v", raw)
	}
}

// TestServeTelemetryEndToEnd runs the real listener: HTTP/2 is negotiated
// for requests (so streams no longer exhaust a browser's six HTTP/1.1
// connections), the watch socket still upgrades over HTTP/1.1, a client that
// rejects the certificate is recorded as a TLS-phase failure, a lost refresh
// answer is recovered by the grace, and shutting down ends open streams as
// server_closed. The counters reach the host through the admin socket.
func TestServeTelemetryEndToEnd(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	certFile, keyFile, pool := writeCert(t, t.TempDir(), "amux.test")
	f := newFake(testRows()...)
	ready := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	var logMu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	go func() {
		served <- Serve(ctx, ServeConfig{
			Root: root, Listen: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile,
			Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
			Backend: f, Verify: h.verify, Logf: logf,
			Ready: func(addr net.Addr, _ string) { ready <- addr },
		})
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-served:
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			if err := <-served; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
	}
	t.Cleanup(stop)
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr.String())
	}
	tr := &http.Transport{DialContext: dial, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "amux.test"}}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}

	// A client that does not trust the certificate.
	untrusted := &http.Client{Transport: &http.Transport{DialContext: dial, TLSClientConfig: &tls.Config{ServerName: "amux.test"}}}
	if _, err := untrusted.Get(testEndpoint + PathSession); err == nil {
		t.Fatal("untrusted TLS accepted")
	}

	adm, err := AdminCall(context.Background(), root, h.host, AdminRequest{Op: OpEnroll, Label: "Pixel", Permissions: []string{PermRead}, Resource: allSessions})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBundle(adm.Bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	postToken := func(v url.Values) (TokenResponse, *http.Response) {
		t.Helper()
		resp, err := client.PostForm(testEndpoint+PathToken, v)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var tok TokenResponse
		_ = json.NewDecoder(resp.Body).Decode(&tok)
		return tok, resp
	}
	tok, resp := postToken(url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {b.Code}, "client_label": {"Pixel"}, "client_kind": {ClientWeb}})
	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s; the listener must offer HTTP/2", resp.Proto)
	}
	if tok.AccessToken == "" {
		t.Fatalf("enroll: %d", resp.StatusCode)
	}
	// A refresh whose answer is lost, then the retry with the same token.
	if _, resp := postToken(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {tok.RefreshToken}}); resp.StatusCode != 200 {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	tok, resp = postToken(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {tok.RefreshToken}})
	if resp.StatusCode != 200 || tok.AccessToken == "" {
		t.Fatalf("grace retry: %d", resp.StatusCode)
	}

	// The watch socket over HTTP/1.1 beside HTTP/2 requests.
	wd := websocket.Dialer{NetDialContext: dial, TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "amux.test"}}
	ws, _, err := wd.Dial("wss://"+testHost+PathWatch, http.Header{"Origin": {testOrigin}})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer ws.Close()
	_ = ws.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	var welcome WatchWelcome
	if err := ws.ReadJSON(&welcome); err != nil || welcome.Type != "welcome" {
		t.Fatalf("welcome: %v %+v", err, welcome)
	}

	// A transcript stream over HTTP/2.
	req, _ := http.NewRequest("GET", testEndpoint+PathSession+"/a1/stream?after=0", nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	stream, err := client.Do(req)
	if err != nil || stream.StatusCode != 200 || stream.ProtoMajor != 2 {
		t.Fatalf("stream: %v %v", err, stream)
	}
	defer stream.Body.Close()
	events := sse(t, stream)
	<-f.streamStarted
	f.feed("a1") <- hp.RuntimeEventBatch{Seq: 1, Runtime: "claude", Events: []hp.RuntimeEvent{{Type: hp.TypeTurnStart}}}
	if e := nextEvent(t, events); e[0] != "events" {
		t.Fatalf("stream: %v", e)
	}

	st, err := AdminCall(context.Background(), root, h.host, AdminRequest{Op: OpCloudStatus})
	if err != nil || st.Connections == nil {
		t.Fatalf("status: %v %+v", err, st)
	}
	c := st.Connections
	if c.TLSFailed < 1 || c.Failures[FailTLSMismatch] < 1 || c.StreamsActive != 2 || c.Refresh.GraceRetry != 1 || c.Refresh.OK != 1 {
		t.Fatalf("connections: %+v", c)
	}

	// Shutting down ends the stream with an end event, recorded as server_closed.
	stop()
	for e := range events {
		if e[0] == "end" && strings.Contains(e[1], CodeUnavailable) {
			break
		}
	}
	logMu.Lock()
	defer logMu.Unlock()
	var sawTLS, sawClosed bool
	for _, l := range logs {
		sawTLS = sawTLS || strings.Contains(l, "phase=tcp ok=false reason=tls_mismatch")
		sawClosed = sawClosed || strings.Contains(l, "route=sessions/{sid}/stream phase=stream ok=true reason=server_closed")
	}
	if !sawTLS || !sawClosed {
		t.Fatalf("log lines missing (tls %v, server_closed %v):\n%s", sawTLS, sawClosed, strings.Join(logs, "\n"))
	}
}
