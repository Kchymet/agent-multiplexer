package direct

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Connection-failure reasons: the stable taxonomy every direct client mirrors
// (docs/direct-access.md, "Connection diagnostics"). A value is never renamed
// or reused for another meaning; new ones may be added, and a client shows an
// unknown one as FailOther. Some are only observable by a client
// (FailUnreachable, FailIdentityMismatch); the server never records them.
const (
	FailUnreachable       = "unreachable"        // no TCP connection: DNS, route, refused, firewall, Local Network Access denied
	FailTLSMismatch       = "tls_mismatch"       // TLS handshake failed: certificate untrusted, wrong name, pin mismatch, version
	FailIdentityMismatch  = "identity_mismatch"  // TLS fine, but the hello proof names another server_id (identity reset, wrong host)
	FailConfigMismatch    = "config_mismatch"    // the dialed host is not a configured endpoint (421), or the origin is not allowed
	FailAuthInvalid       = "auth_invalid"       // unknown access token (typically after a listener restart): refresh once
	FailAuthExpired       = "auth_expired"       // the access token's lifetime ended (or the grant changed): refresh, reconnect
	FailAuthRevoked       = "auth_revoked"       // the grant is gone (revoked, expired, reused refresh token): enroll again
	FailRateLimited       = "rate_limited"       // 429 from the per-source failure limiter
	FailTimeout           = "timeout"            // a handshake, read, write or keepalive deadline passed
	FailServerClosed      = "server_closed"      // the listener stopped or restarted under an open connection
	FailServerUnavailable = "server_unavailable" // the listener answered, but the amux daemon behind it did not
	FailNetworkChanged    = "network_changed"    // a local network change (client), or the listen address left this host
	FailClientClosed      = "client_closed"      // the client went away (a normal end for a stream)
	FailProtocol          = "protocol"           // malformed or unsupported request (plain HTTP to HTTPS, bad hello)
	FailOther             = "other"
)

// FailureReasons is the taxonomy in documentation order.
var FailureReasons = []string{
	FailUnreachable, FailTLSMismatch, FailIdentityMismatch, FailConfigMismatch, FailAuthInvalid, FailAuthExpired,
	FailAuthRevoked, FailRateLimited, FailTimeout, FailServerClosed, FailServerUnavailable, FailNetworkChanged,
	FailClientClosed, FailProtocol, FailOther,
}

// Connection phases, in order. An attempt records the furthest one it reached.
const (
	PhaseTCP    = "tcp"    // a TCP connection was accepted
	PhaseTLS    = "tls"    // the TLS handshake completed
	PhaseHTTP   = "http"   // a request passed the Host and Origin gates
	PhaseAuth   = "auth"   // a bearer token (or a refresh / enrollment exchange) was accepted
	PhaseStream = "stream" // a live stream (SSE transcript tail or watch socket) was established
)

// ConnAttempt is one recorded connection attempt: a TLS-level failure, an
// unauthenticated exchange (hello, token, claim), a refused request, or a
// live stream from open to end. It carries no token, code or request body.
type ConnAttempt struct {
	AtMS       int64  `json:"at_ms"` // unix milliseconds when the attempt started
	Route      string `json:"route,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	Remote     string `json:"remote,omitempty"`
	Phase      string `json:"phase"` // furthest phase reached
	OK         bool   `json:"ok"`
	Reason     string `json:"reason,omitempty"` // a Fail* value; for a stream, why it ended
	Code       string `json:"code,omitempty"`   // the wire code or refusal detail, when there is one
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"` // e.g. the TLS error text
}

// String renders the attempt as one logfmt line for the listener's log.
func (c ConnAttempt) String() string {
	var b strings.Builder
	kv := func(k, v string) {
		if v == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		if strings.ContainsAny(v, " \"=\t") {
			v = strconv.Quote(v)
		}
		b.WriteString(v)
	}
	kv("route", c.Route)
	kv("phase", c.Phase)
	kv("ok", strconv.FormatBool(c.OK))
	kv("reason", c.Reason)
	kv("code", c.Code)
	if c.Status != 0 {
		kv("status", strconv.Itoa(c.Status))
	}
	kv("client", c.ClientID)
	kv("remote", c.Remote)
	kv("dur_ms", strconv.FormatInt(c.DurationMS, 10))
	kv("detail", c.Detail)
	return b.String()
}

// ListenStats is the listener's view of its own address.
type ListenStats struct {
	Addr string `json:"addr,omitempty"`
	// Present says the listen IP is still assigned to an interface of this
	// host (always true for an unspecified bind). False means clients cannot
	// reach this listener until the address returns or the listener is
	// reconfigured: the LAN or VPN interface changed.
	Present    bool  `json:"present"`
	CheckedAt  int64 `json:"checked_at,omitempty"`
	ChangedAt  int64 `json:"changed_at,omitempty"`
	Changes    int64 `json:"changes"`
	Configured bool  `json:"-"`
}

// ConnStats are the listener's connection counters since Since (when this
// process started serving), surfaced by `amux device status --json` as
// server.connections. They are memory only.
type ConnStats struct {
	Since         int64 `json:"since"`
	TCPAccepted   int64 `json:"tcp_accepted"`
	TLSOK         int64 `json:"tls_ok"`
	TLSFailed     int64 `json:"tls_failed"`
	Requests      int64 `json:"requests"`
	Authenticated int64 `json:"authenticated"`
	StreamsOpened int64 `json:"streams_opened"`
	StreamsActive int64 `json:"streams_active"`
	// Failures counts failed attempts by reason, FailuresByPhase by the
	// furthest phase they reached. StreamEnds counts why streams ended
	// (client_closed and auth_expired are normal ends).
	Failures        map[string]int64 `json:"failures"`
	FailuresByPhase map[string]int64 `json:"failures_by_phase"`
	StreamEnds      map[string]int64 `json:"stream_ends"`
	Refresh         RefreshStats     `json:"refresh"`
	Listen          ListenStats      `json:"listen"`
	// Recent holds the latest recorded attempts (failures, exchanges and
	// stream ends), oldest first, at most maxRecentAttempts.
	Recent []ConnAttempt `json:"recent,omitempty"`
}

const (
	maxRecentAttempts = 64
	maxPendingTLSErrs = 1024
)

// connTel collects connection telemetry for one Server. Every method is a
// short critical section on its own mutex, never held across I/O; the log
// line is written after it is released.
type connTel struct {
	now  func() time.Time
	logf func(string, ...any)

	mu      sync.Mutex
	st      ConnStats
	recent  []ConnAttempt
	conns   map[net.Conn]*connTrack
	tlsErrs map[string]string // remote addr -> handshake error, from the server's error log
}

type connTrack struct {
	start  time.Time
	remote string
	tlsOK  bool
}

func newConnTel(now func() time.Time, logf func(string, ...any)) *connTel {
	return &connTel{
		now: now, logf: logf,
		st:    ConnStats{Since: now().Unix(), Failures: map[string]int64{}, FailuresByPhase: map[string]int64{}, StreamEnds: map[string]int64{}},
		conns: map[net.Conn]*connTrack{}, tlsErrs: map[string]string{},
	}
}

// snapshot copies the counters.
func (t *connTel) snapshot() ConnStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.st
	st.Failures, st.FailuresByPhase, st.StreamEnds = cloneCounts(t.st.Failures), cloneCounts(t.st.FailuresByPhase), cloneCounts(t.st.StreamEnds)
	st.Recent = append([]ConnAttempt(nil), t.recent...)
	return st
}

func cloneCounts(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// record files a finished attempt in the counters and the recent ring, then
// logs it.
func (t *connTel) record(a ConnAttempt, stream bool) {
	t.mu.Lock()
	if stream {
		t.st.StreamEnds[a.Reason]++
	}
	if !a.OK {
		t.st.Failures[a.Reason]++
		t.st.FailuresByPhase[a.Phase]++
	}
	t.recent = append(t.recent, a)
	if len(t.recent) > maxRecentAttempts {
		t.recent = append(t.recent[:0], t.recent[len(t.recent)-maxRecentAttempts:]...)
	}
	t.mu.Unlock()
	t.logf("direct: conn %s", a)
}

// ── TCP / TLS (http.Server hooks) ──────────────────────────────────────────

// connState is http.Server.ConnState. A connection that closes before its
// TLS handshake completed is a TLS-phase failure, classified from the error
// the server logged for it (errorLog) just before.
func (t *connTel) connState(c net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		t.mu.Lock()
		t.st.TCPAccepted++
		t.conns[c] = &connTrack{start: t.now(), remote: c.RemoteAddr().String()}
		t.mu.Unlock()
	case http.StateActive, http.StateIdle:
		t.mu.Lock()
		if tr := t.conns[c]; tr != nil && !tr.tlsOK {
			tr.tlsOK = true
			t.st.TLSOK++
		}
		t.mu.Unlock()
	case http.StateHijacked, http.StateClosed:
		t.mu.Lock()
		tr := t.conns[c]
		delete(t.conns, c)
		var tlsErr string
		if tr != nil {
			tlsErr = t.tlsErrs[tr.remote]
			delete(t.tlsErrs, tr.remote)
		}
		t.mu.Unlock()
		if tr == nil || tr.tlsOK {
			return
		}
		if tc, ok := c.(*tls.Conn); ok && tlsErr == "" && tc.ConnectionState().HandshakeComplete {
			// Handshake done, closed before any request (a speculative
			// preconnect, or an idle keep-alive): not a failure.
			t.mu.Lock()
			t.st.TLSOK++
			t.mu.Unlock()
			return
		}
		reason := FailClientClosed
		if tlsErr != "" {
			reason = classifyTLSError(tlsErr)
		}
		t.mu.Lock()
		t.st.TLSFailed++
		t.mu.Unlock()
		t.record(ConnAttempt{
			AtMS: tr.start.UnixMilli(), Remote: tr.remote, Phase: PhaseTCP, Reason: reason,
			DurationMS: t.now().Sub(tr.start).Milliseconds(), Detail: tlsErr,
		}, false)
	}
}

// classifyTLSError maps a server-side TLS handshake error to the taxonomy.
func classifyTLSError(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "timeout"), strings.Contains(m, "deadline exceeded"):
		return FailTimeout
	case strings.Contains(m, "http request to an https server"), strings.Contains(m, "does not look like a tls handshake"),
		strings.Contains(m, "oversized record"):
		return FailProtocol
	case strings.Contains(m, "eof"), strings.Contains(m, "connection reset"), strings.Contains(m, "broken pipe"):
		// The client hung up mid-handshake. Browsers and pinning clients often
		// do this instead of sending an alert when they reject a certificate,
		// so it is the usual signature of an untrusted or mismatched one too.
		return FailClientClosed
	case strings.Contains(m, "certificate"), strings.Contains(m, "unsupported versions"), strings.Contains(m, "cipher"),
		strings.Contains(m, "application protocol"), strings.Contains(m, "remote error"), strings.Contains(m, "tls:"):
		return FailTLSMismatch
	}
	return FailOther
}

// errorLog is the http.Server's ErrorLog sink. A TLS handshake error is kept
// for connState to classify and record (it runs right after, on the same
// goroutine); anything else goes to the listener's log as before.
type errorLog struct{ t *connTel }

const tlsHandshakePrefix = "http: TLS handshake error from "

func (e errorLog) Write(p []byte) (int, error) {
	sc := bufio.NewScanner(bytes.NewReader(p))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, tlsHandshakePrefix); ok {
			// "<remote>: <error>"; the remote is host:port, IPv6 in brackets.
			if i := strings.Index(rest, ": "); i > 0 {
				e.t.mu.Lock()
				if len(e.t.tlsErrs) < maxPendingTLSErrs {
					e.t.tlsErrs[rest[:i]] = rest[i+2:]
				}
				e.t.mu.Unlock()
				continue
			}
		}
		e.t.logf("direct: %s", line)
	}
	return len(p), nil
}

// ── requests ───────────────────────────────────────────────────────────────

// attempt is one request in flight. Handlers advance its phase and, for a
// stream, say why it ended; finish classifies the rest from the answer.
type attempt struct {
	t       *connTel
	start   time.Time
	remote  string
	mu      sync.Mutex
	pattern string
	phase   string
	client  string
	stream  bool   // reached PhaseStream
	endWhy  string // stream end reason
	endCode string
	failWhy string // an explicit failure (a watch refused before any status)
	code    string // the wire code written, if any
	detail  string
	status  int
	hdrSent bool
}

type attemptKey struct{}

func attemptOf(ctx context.Context) *attempt { a, _ := ctx.Value(attemptKey{}).(*attempt); return a }

func (t *connTel) begin(r *http.Request) *attempt {
	t.mu.Lock()
	t.st.Requests++
	t.mu.Unlock()
	return &attempt{t: t, start: t.now(), remote: r.RemoteAddr, phase: PhaseTLS}
}

func (a *attempt) reach(phase, clientID string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if phaseRank(phase) > phaseRank(a.phase) {
		a.phase = phase
	}
	if clientID != "" {
		a.client = clientID
	}
	switch phase {
	case PhaseAuth:
		a.t.mu.Lock()
		a.t.st.Authenticated++
		a.t.mu.Unlock()
	case PhaseStream:
		if !a.stream {
			a.stream = true
			a.t.mu.Lock()
			a.t.st.StreamsOpened++
			a.t.st.StreamsActive++
			a.t.mu.Unlock()
		}
	}
}

// ended records why a stream ended (the first reason given wins).
func (a *attempt) ended(reason, code string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.endWhy == "" {
		a.endWhy, a.endCode = reason, code
	}
}

// fail records an explicit failure reason (first wins).
func (a *attempt) fail(reason, code string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failWhy == "" {
		a.failWhy, a.code = reason, code
	}
}

// note records a detail for the log (e.g. a refresh refusal's reason).
func (a *attempt) note(detail string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.detail = detail
	a.mu.Unlock()
}

func phaseRank(p string) int {
	switch p {
	case PhaseTCP:
		return 1
	case PhaseTLS:
		return 2
	case PhaseHTTP:
		return 3
	case PhaseAuth:
		return 4
	case PhaseStream:
		return 5
	}
	return 0
}

// loggedRoutes are recorded and logged on success too: the exchanges a
// client makes to connect (each is a connection attempt in its own right).
// Other routes are counted, and recorded only when they fail.
var loggedRoutes = map[string]bool{"hello": true, "token": true, "claim": true, "revoke": true, "enroll/cloud": true}

func (a *attempt) setPattern(p string) {
	a.mu.Lock()
	a.pattern = p
	a.mu.Unlock()
}

// finish classifies and records the attempt once its handler returned.
func (a *attempt) finish() {
	a.mu.Lock()
	route := routeName(a.pattern)
	at := ConnAttempt{
		AtMS: a.start.UnixMilli(), Route: route, ClientID: a.client, Remote: a.remote, Phase: a.phase,
		Status: a.status, Code: a.code, DurationMS: a.t.now().Sub(a.start).Milliseconds(), Detail: a.detail,
	}
	stream, endWhy, endCode, failWhy := a.stream, a.endWhy, a.endCode, a.failWhy
	a.mu.Unlock()
	if stream {
		a.t.mu.Lock()
		a.t.st.StreamsActive--
		a.t.mu.Unlock()
		if endWhy == "" {
			endWhy = FailClientClosed
		}
		at.Reason, at.OK = endWhy, true
		if endCode != "" {
			at.Code = endCode
		}
		a.t.record(at, true)
		return
	}
	at.Reason = failWhy
	if at.Reason == "" {
		at.Reason = classifyAnswer(route, at.Status, at.Code)
	}
	at.OK = at.Reason == ""
	if at.OK && !loggedRoutes[route] {
		return
	}
	a.t.record(at, false)
}

// routeName is the stable route label: the mux pattern without its method
// and the /direct/v1/ prefix ("sessions/{sid}/stream"), or "unrouted".
func routeName(pattern string) string {
	if pattern == "" {
		return "unrouted"
	}
	if _, p, ok := strings.Cut(pattern, " "); ok {
		pattern = p
	}
	return strings.TrimPrefix(pattern, "/direct/v1/")
}

// classifyAnswer maps a non-stream answer to a failure reason, or "" when the
// connection itself worked (including application refusals such as
// not_found, denied or an idempotency conflict: they are answers, not
// connection failures).
func classifyAnswer(route string, status int, code string) string {
	switch code {
	case CodeAuthInvalid:
		return FailAuthInvalid
	case CodeAuthExpired, CodeGrantChanged:
		return FailAuthExpired
	case CodeAuthRevoked:
		return FailAuthRevoked
	case CodeEndpointNotBound, CodeOriginNotAllowed:
		return FailConfigMismatch
	case CodeProtocol:
		return FailProtocol
	case CodeUnavailable:
		return FailServerUnavailable
	case CodeRateLimited:
		return FailRateLimited
	case OAuthInvalidGrant:
		if route == "token" {
			// Refresh or enrollment code refused: the client cannot continue
			// on this grant (or code) and must enroll again.
			return FailAuthRevoked
		}
		return FailAuthInvalid
	case OAuthTemporarilyUnavail:
		if status == http.StatusTooManyRequests {
			return FailRateLimited
		}
		return FailServerUnavailable
	}
	switch {
	case status == http.StatusTooManyRequests:
		return FailRateLimited
	case status == http.StatusMisdirectedRequest:
		return FailConfigMismatch
	case status >= 500:
		return FailServerUnavailable
	case route == "hello" && status == http.StatusBadRequest:
		return FailProtocol
	}
	return ""
}

// telWriter observes the status and wire code a handler writes. It keeps
// Flush, Hijack and the ResponseController (Unwrap) working underneath.
type telWriter struct {
	http.ResponseWriter
	a *attempt
}

func (w *telWriter) WriteHeader(code int) {
	w.a.mu.Lock()
	if !w.a.hdrSent {
		w.a.hdrSent, w.a.status = true, code
	}
	w.a.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *telWriter) Write(p []byte) (int, error) {
	w.a.mu.Lock()
	if !w.a.hdrSent {
		w.a.hdrSent, w.a.status = true, http.StatusOK
	}
	w.a.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

func (w *telWriter) noteCode(code string) {
	w.a.mu.Lock()
	if w.a.code == "" {
		w.a.code = code
	}
	w.a.mu.Unlock()
}

func (w *telWriter) Flush() { _ = w.FlushError() }

// FlushError keeps http.ResponseController.Flush reporting a dead connection:
// the controller prefers it over Flush, which cannot return the error.
func (w *telWriter) FlushError() error { return http.NewResponseController(w.ResponseWriter).Flush() }

func (w *telWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	w.a.mu.Lock()
	if !w.a.hdrSent {
		w.a.hdrSent, w.a.status = true, http.StatusSwitchingProtocols
	}
	w.a.mu.Unlock()
	return h.Hijack()
}

func (w *telWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// noteCode tells the attempt behind w which wire code was answered.
func noteCode(w http.ResponseWriter, code string) {
	if n, ok := w.(interface{ noteCode(string) }); ok {
		n.noteCode(code)
	}
}

// ── listen address ─────────────────────────────────────────────────────────

// addrAssigned reports whether ip is assigned to an interface of this host.
// An unspecified address is always present. A lookup error counts as present
// (nothing is known to have changed).
func addrAssigned(ip net.IP, addrs func() ([]net.Addr, error)) bool {
	if ip == nil || ip.IsUnspecified() {
		return true
	}
	list, err := addrs()
	if err != nil {
		return true
	}
	for _, a := range list {
		var got net.IP
		switch v := a.(type) {
		case *net.IPNet:
			got = v.IP
		case *net.IPAddr:
			got = v.IP
		}
		if got != nil && got.Equal(ip) {
			return true
		}
	}
	return false
}

// listenCheck records whether the listen address is still assigned, and logs
// and records a network_changed attempt when that changes.
func (t *connTel) listenCheck(addr string, present bool) {
	now := t.now()
	t.mu.Lock()
	prev, first := t.st.Listen.Present, !t.st.Listen.Configured
	t.st.Listen.Addr, t.st.Listen.Present, t.st.Listen.CheckedAt, t.st.Listen.Configured = addr, present, now.Unix(), true
	changed := !first && prev != present
	if changed {
		t.st.Listen.ChangedAt = now.Unix()
		t.st.Listen.Changes++
	}
	t.mu.Unlock()
	if !changed && !(first && !present) {
		return // log a change, or a first check that finds the address missing
	}
	detail := "the listen address is assigned to this host again"
	if !present {
		detail = "the listen address is no longer assigned to any interface of this host; clients cannot reach this listener until it returns (the LAN or VPN interface changed)"
	}
	t.record(ConnAttempt{AtMS: now.UnixMilli(), Route: "listen", Remote: addr, Phase: PhaseTCP, OK: present,
		Reason: FailNetworkChanged, Detail: detail}, false)
}

// watchListen re-checks the listen address every interval until ctx ends.
func (t *connTel) watchListen(ctx context.Context, addr string, interval time.Duration, addrs func() ([]net.Addr, error)) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	t.listenCheck(addr, addrAssigned(ip, addrs))
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.listenCheck(addr, addrAssigned(ip, addrs))
		}
	}
}

// String summarizes the counters in one human line (amux device status).
func (st ConnStats) String() string {
	var fails []string
	for _, r := range FailureReasons {
		if n := st.Failures[r]; n > 0 {
			fails = append(fails, fmt.Sprintf("%s %d", r, n))
		}
	}
	s := fmt.Sprintf("tcp %d · tls ok %d failed %d · requests %d · authenticated %d · streams %d open %d",
		st.TCPAccepted, st.TLSOK, st.TLSFailed, st.Requests, st.Authenticated, st.StreamsOpened, st.StreamsActive)
	if len(fails) > 0 {
		s += " · failures: " + strings.Join(fails, ", ")
	}
	return s
}
