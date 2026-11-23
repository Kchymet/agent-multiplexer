package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Placeholder approvals: the right shape, obviously not real.
var (
	testApproval  = hp.DirectApprovalPrefix + strings.Repeat("A", 43)
	testApproval2 = hp.DirectApprovalPrefix + strings.Repeat("B", 43)
)

// fakeHarness plays the provider + Harness: it leases parked verifies from
// the queue and answers each with answer(item), counting verifies.
type fakeHarness struct {
	mu     sync.Mutex
	answer func(ApprovalItem) ApprovalVerdict
	seen   []hp.DirectApprovalVerify
	calls  atomic.Int32
}

func (f *fakeHarness) run(t *testing.T, q *ApprovalQueue) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q.touch() // present before the first request
	go func() {
		for ctx.Err() == nil {
			for _, it := range q.Lease(ctx, MaxApprovalsInFlight, 50*time.Millisecond) {
				f.calls.Add(1)
				f.mu.Lock()
				f.seen = append(f.seen, it.Verify)
				ans := f.answer
				f.mu.Unlock()
				v := ans(it)
				v.ID = it.ID
				q.Settle([]ApprovalVerdict{v})
			}
		}
	}()
}

func (f *fakeHarness) set(ans func(ApprovalItem) ApprovalVerdict) {
	f.mu.Lock()
	f.answer = ans
	f.mu.Unlock()
}

func okVerdict(perms []string, res *hp.DirectResource, handoff string) func(ApprovalItem) ApprovalVerdict {
	return func(ApprovalItem) ApprovalVerdict {
		return ApprovalVerdict{ProviderID: testProvider, Result: hp.DirectApprovalResult{Requester: testOwner, Permissions: perms,
			Resource: res, Label: "Chrome on laptop", Handoff: handoff, ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}}
	}
}

func errVerdict(code string) func(ApprovalItem) ApprovalVerdict {
	return func(ApprovalItem) ApprovalVerdict {
		return ApprovalVerdict{ProviderID: testProvider, Result: hp.DirectApprovalResult{Error: code}}
	}
}

type approvalRig struct {
	t     *testing.T
	a     *Authority
	q     *ApprovalQueue
	h     *fakeHarness
	logs  *[]string
	logMu *sync.Mutex
}

func newApprovalRig(t *testing.T, c *clock) *approvalRig {
	a, _ := enabledAuthority(t, c) // ceiling read,steer on wg1, wg2
	q := NewApprovalQueue()
	h := &fakeHarness{answer: okVerdict([]string{PermRead, PermSteer}, &hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"wg1"}}, "")}
	h.run(t, q)
	var logs []string
	return &approvalRig{t: t, a: a, q: q, h: h, logs: &logs, logMu: &sync.Mutex{}}
}

// server builds a fresh server per call, so the per-source failure limit
// never interferes unless a test wants it to.
func (r *approvalRig) server() *Server {
	s, err := NewServer(Config{Authority: r.a, Backend: newFake(), Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
		WatchPoll: 20 * time.Millisecond, Revalidate: time.Hour, Keepalive: time.Hour, Approvals: r.q,
		Logf: func(f string, v ...any) {
			r.logMu.Lock()
			*r.logs = append(*r.logs, fmt.Sprintf(f, v...))
			r.logMu.Unlock()
		}})
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

type cloudAnswer struct {
	status int
	ok     EnrollCloudResponse
	err    OAuthError
	hdr    http.Header
}

func (r *approvalRig) post(s *Server, approval, challenge string, hdr ...string) cloudAnswer {
	v := url.Values{"approval": {approval}, "code_challenge": {challenge}, "code_challenge_method": {CodeChallengeMethodS256}}
	w := do(s, http.MethodPost, PathEnrollCloud, "", form(v), append([]string{"Content-Type", formCT}, hdr...)...)
	out := cloudAnswer{status: w.Code, hdr: w.Header()}
	if w.Code == http.StatusOK {
		_ = json.Unmarshal(w.Body.Bytes(), &out.ok)
	} else {
		_ = json.Unmarshal(w.Body.Bytes(), &out.err)
	}
	if strings.Contains(w.Body.String(), approval) && approval != "" {
		r.t.Fatalf("response echoes the approval: %s", w.Body)
	}
	return out
}

// The full tailnet flow: approval -> code -> token, idempotent per
// (approval, challenge), never verified twice for a replay.
func TestEnrollCloudIssuesCodeThenTokenRedeems(t *testing.T) {
	r := newApprovalRig(t, nil)
	got := r.post(r.server(), testApproval, testChallenge)
	if got.status != http.StatusOK || got.ok.Code == "" || got.ok.ServerID != r.a.ServerID() || got.ok.ExpiresAt == 0 {
		t.Fatalf("enroll/cloud: %+v", got)
	}
	if got.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("not no-store")
	}
	r.h.mu.Lock()
	seen := r.h.seen[0]
	r.h.mu.Unlock()
	if seen != (hp.DirectApprovalVerify{Approval: testApproval, ServerID: r.a.ServerID(), CodeChallenge: testChallenge}) {
		t.Fatalf("verify sent %+v", seen)
	}
	// Same approval + same challenge: the same code, no second verify.
	if again := r.post(r.server(), testApproval, testChallenge); again.status != http.StatusOK || again.ok.Code != got.ok.Code {
		t.Fatalf("replay: %+v", again)
	}
	if r.h.calls.Load() != 1 {
		t.Fatalf("verified %d times", r.h.calls.Load())
	}
	// Another challenge for the same approval: used.
	if other := r.post(r.server(), testApproval, S256Challenge(otherVerifier)); other.status != http.StatusForbidden ||
		other.err.Error != OAuthAccessDenied || other.err.Reason != ReasonTicketUsed {
		t.Fatalf("other challenge: %+v", other)
	}
	// Redeem with the verifier at the existing token endpoint.
	tok := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {got.ok.Code}, "client_label": {"Chrome"}, "client_kind": {ClientWeb}, "code_verifier": {testVerifier}}
	if w := do(r.server(), http.MethodPost, PathToken, "", form(tok), "Content-Type", formCT); w.Code != http.StatusOK {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	clients := r.a.List()
	if len(clients) != 1 || strings.Join(clients[0].Permissions, ",") != "read,steer" || clients[0].Origin != OriginCloud || clients[0].CloudRequester != testOwner {
		t.Fatalf("grant %+v", clients)
	}
	// After redemption the approval is used.
	if after := r.post(r.server(), testApproval, testChallenge); after.status != http.StatusForbidden || after.err.Reason != ReasonTicketUsed {
		t.Fatalf("after redeem: %+v", after)
	}
	// Recorded as an exchange, and never logged with the approval.
	act := r.a.EnrollActivity()
	if act.Last == nil || act.Last.Step != EnrollStepCloud || act.Last.Reason != ReasonTicketUsed {
		t.Fatalf("exchange record %+v", act.Last)
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	joined := strings.Join(*r.logs, "\n")
	if strings.Contains(joined, testApproval) || strings.Contains(joined, got.ok.Code) || !strings.Contains(joined, "enroll_cloud") {
		t.Fatalf("logs:\n%s", joined)
	}
}

// A device-ticket approval (QR v3) is unbound: the scanning device's own
// challenge is what the code is bound to.
func TestEnrollCloudHandoffApprovalBindsDeviceChallenge(t *testing.T) {
	r := newApprovalRig(t, nil)
	r.h.set(okVerdict([]string{PermRead}, &hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"wg2"}}, hp.DirectHandoffDeviceTicket))
	deviceChallenge := S256Challenge(otherVerifier)
	got := r.post(r.server(), testApproval2, deviceChallenge)
	if got.status != http.StatusOK {
		t.Fatalf("handoff: %+v", got)
	}
	tok := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {got.ok.Code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}, "code_verifier": {otherVerifier}}
	if w := do(r.server(), http.MethodPost, PathToken, "", form(tok), "Content-Type", formCT); w.Code != http.StatusOK {
		t.Fatalf("token: %d %s", w.Code, w.Body)
	}
	// The QR v3 payload round-trips.
	q, err := EncodeApprovalBundle(ApprovalBundle{ServerID: r.a.ServerID(), Endpoints: []string{testEndpoint}, Approval: testApproval2, ExpiresAt: time.Now().Add(time.Minute).Unix()})
	if err != nil || !strings.HasPrefix(q, BundlePrefixApproval) {
		t.Fatalf("encode %q %v", q, err)
	}
	b, err := DecodeApprovalBundle(q, time.Now())
	if err != nil || b.V != 3 || b.Approval != testApproval2 || b.ServerID != r.a.ServerID() {
		t.Fatalf("decode %+v %v", b, err)
	}
	if _, err := DecodeApprovalBundle(q, time.Now().Add(2*time.Minute)); err == nil {
		t.Fatal("expired v3 accepted")
	}
	if _, err := DecodeApprovalBundle(BundlePrefixApproval+"e30", time.Now()); err == nil {
		t.Fatal("empty v3 accepted")
	}
}

// Every refusal has exactly the contract's status, error and reason.
func TestEnrollCloudRefusals(t *testing.T) {
	r := newApprovalRig(t, nil)
	wgOver := &hp.DirectResource{Kind: hp.DirectResourceAllSessions}
	cases := []struct {
		name    string
		answer  func(ApprovalItem) ApprovalVerdict
		status  int
		err     string
		reason  string
		mutate  func(*approvalRig)
		request func(*approvalRig, *Server) cloudAnswer
	}{
		{name: "approval_unknown", answer: errVerdict(hp.DirectApprovalErrUnknown), status: 403, err: OAuthAccessDenied, reason: ReasonApprovalUnknown},
		{name: "approval_expired", answer: errVerdict(hp.DirectApprovalErrExpired), status: 403, err: OAuthAccessDenied, reason: ReasonTicketExpired},
		{name: "approval_used", answer: errVerdict(hp.DirectApprovalErrUsed), status: 403, err: OAuthAccessDenied, reason: ReasonTicketUsed},
		{name: "approval_mismatch", answer: errVerdict(hp.DirectApprovalErrMismatch), status: 403, err: OAuthAccessDenied, reason: ReasonApprovalMismatch},
		{name: "owner_changed", answer: errVerdict(hp.DirectApprovalErrOwnerChange), status: 403, err: OAuthAccessDenied, reason: ReasonOwnerMismatch},
		{name: "harness unavailable", answer: errVerdict(hp.DirectApprovalErrUnavailable), status: 503, err: OAuthTemporarilyUnavail, reason: ReasonUnavailable},
		{name: "owner pin", answer: func(it ApprovalItem) ApprovalVerdict {
			v := okVerdict([]string{PermRead}, nil, "")(it)
			v.Result.Requester = "someone_else"
			return v
		}, status: 403, err: OAuthAccessDenied, reason: ReasonOwnerMismatch},
		{name: "provider pin", answer: func(it ApprovalItem) ApprovalVerdict {
			v := okVerdict([]string{PermRead}, nil, "")(it)
			v.ProviderID = "prov_other"
			return v
		}, status: 403, err: OAuthAccessDenied, reason: ReasonProviderMismatch},
		{name: "scope over ceiling", answer: okVerdict([]string{PermRead}, wgOver, ""), status: 403, err: OAuthAccessDenied, reason: ReasonCeiling},
		{name: "perms over ceiling", answer: okVerdict([]string{PermRead, PermManage}, nil, ""), status: 403, err: OAuthAccessDenied, reason: ReasonCeiling},
		{name: "console", answer: okVerdict([]string{PermRead}, &hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{ConsoleID}}, ""),
			status: 403, err: OAuthAccessDenied, reason: ReasonApprovalMismatch},
		{name: "unknown handoff", answer: okVerdict([]string{PermRead}, nil, "bearer"), status: 403, err: OAuthAccessDenied, reason: ReasonApprovalMismatch},
		{name: "malformed approval", status: 400, err: OAuthInvalidRequest, reason: ReasonMalformed,
			request: func(r *approvalRig, s *Server) cloudAnswer { return r.post(s, "hdap_short", testChallenge) }},
		{name: "malformed challenge", status: 400, err: OAuthInvalidRequest, reason: ReasonMalformed,
			request: func(r *approvalRig, s *Server) cloudAnswer { return r.post(s, testApproval, "nope") }},
		{name: "query string", status: 400, err: OAuthInvalidRequest, reason: ReasonMalformed,
			request: func(r *approvalRig, s *Server) cloudAnswer {
				w := do(s, http.MethodPost, PathEnrollCloud+"?approval="+testApproval, "", form(url.Values{"code_challenge": {testChallenge}, "code_challenge_method": {"S256"}}), "Content-Type", formCT)
				var e OAuthError
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				return cloudAnswer{status: w.Code, err: e}
			}},
		{name: "plain method", status: 400, err: OAuthInvalidRequest, reason: ReasonMalformed,
			request: func(r *approvalRig, s *Server) cloudAnswer {
				w := do(s, http.MethodPost, PathEnrollCloud, "", form(url.Values{"approval": {testApproval}, "code_challenge": {testChallenge}, "code_challenge_method": {"plain"}}), "Content-Type", formCT)
				var e OAuthError
				_ = json.Unmarshal(w.Body.Bytes(), &e)
				return cloudAnswer{status: w.Code, err: e}
			}},
	}
	for i, c := range cases {
		if c.answer != nil {
			r.h.set(c.answer)
		}
		approval := hp.DirectApprovalPrefix + fmt.Sprintf("%043d", i)
		var got cloudAnswer
		if c.request != nil {
			got = c.request(r, r.server())
		} else {
			got = r.post(r.server(), approval, testChallenge)
		}
		if got.status != c.status || got.err.Error != c.err || got.err.Reason != c.reason {
			t.Errorf("%s: %d %+v, want %d %s/%s", c.name, got.status, got.err, c.status, c.err, c.reason)
		}
	}
}

func TestEnrollCloudPolicyAndReachability(t *testing.T) {
	// Cloud enrollment off: refused before any verify.
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	q := NewApprovalQueue()
	h := &fakeHarness{answer: okVerdict([]string{PermRead}, nil, "")}
	h.run(t, q)
	r := &approvalRig{t: t, a: a, q: q, h: h, logs: &[]string{}, logMu: &sync.Mutex{}}
	if got := r.post(r.server(), testApproval, testChallenge); got.status != 403 || got.err.Reason != ReasonCloudDisabled || h.calls.Load() != 0 {
		t.Fatalf("disabled: %+v (verifies %d)", got, h.calls.Load())
	}

	// No provider polling: orchestrator_unreachable at once.
	a2, _ := enabledAuthority(t, nil)
	idle := &approvalRig{t: t, a: a2, q: NewApprovalQueue(), logs: &[]string{}, logMu: &sync.Mutex{}}
	start := time.Now()
	if got := idle.post(idle.server(), testApproval, testChallenge); got.status != 503 || got.err.Error != OAuthTemporarilyUnavail || got.err.Reason != ReasonOrchestratorUnreachable {
		t.Fatalf("no provider: %+v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waited for an absent provider")
	}

	// A provider that leases but never answers: bounded wait, then 503.
	old := approvalWaitTimeout
	approvalWaitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { approvalWaitTimeout = old })
	silent := NewApprovalQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	silent.touch()
	go func() {
		for ctx.Err() == nil {
			silent.Lease(ctx, 4, 50*time.Millisecond)
		}
	}()
	s, _ := NewServer(Config{Authority: a2, Backend: newFake(), Endpoints: []string{testEndpoint}, Approvals: silent, Logf: func(string, ...any) {}})
	if got := idle.post(s, testApproval, testChallenge); got.status != 503 || got.err.Reason != ReasonOrchestratorUnreachable {
		t.Fatalf("silent provider: %+v", got)
	}

	// Rate limit: 429 with Retry-After.
	lim, _ := NewServer(Config{Authority: a2, Backend: newFake(), Endpoints: []string{testEndpoint}, Approvals: NewApprovalQueue(), Logf: func(string, ...any) {}})
	var last cloudAnswer
	for i := 0; i < 50; i++ {
		last = idle.post(lim, hp.DirectApprovalPrefix+fmt.Sprintf("%043d", i), testChallenge)
		if last.status == http.StatusTooManyRequests {
			break
		}
		// Feed the limiter with refusals the way a guessing client would.
		lim.recordFailure("192.0.2.1")
	}
	if last.status != http.StatusTooManyRequests || last.err.Reason != ReasonRateLimited || last.hdr.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %+v", last)
	}
}

func TestApprovalQueueBounds(t *testing.T) {
	q := NewApprovalQueue()
	q.touch()
	old := approvalWaitTimeout
	approvalWaitTimeout = 500 * time.Millisecond
	t.Cleanup(func() { approvalWaitTimeout = old })
	var wg sync.WaitGroup
	for i := 0; i < MaxApprovalsInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = q.Verify(context.Background(), hp.DirectApprovalVerify{Approval: testApproval})
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.pending)
		q.mu.Unlock()
		if n == MaxApprovalsInFlight || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := q.Verify(context.Background(), hp.DirectApprovalVerify{Approval: testApproval}); err != errApprovalBusy {
		t.Fatalf("fifth verify: %v", err)
	}
	items := q.Lease(context.Background(), 2, 0)
	if len(items) != 2 {
		t.Fatalf("leased %d", len(items))
	}
	if more := q.Lease(context.Background(), 10, 0); len(more) != 2 {
		t.Fatalf("leased %d more (each item is leased once)", len(more))
	}
	wg.Wait()
	// A stale provider (no poll within approvalPollFresh): unreachable at once.
	q.mu.Lock()
	q.lastPoll = time.Now().Add(-time.Hour)
	q.mu.Unlock()
	if _, err := q.Verify(context.Background(), hp.DirectApprovalVerify{Approval: testApproval}); err != errApprovalUnreachable {
		t.Fatalf("stale provider: %v", err)
	}
}

// The relay bridge advertises approval and the ceiling only from a direct
// server that serves approvals, and carries lease/settle over the admin ops.
func TestRelayIssuerApprovalOfferAndBridge(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	q := NewApprovalQueue()
	eps := []string{testEndpoint}
	call := func(served bool) AdminCaller {
		return func(ctx context.Context, req AdminRequest) (AdminResponse, error) {
			var aq *ApprovalQueue
			if served {
				aq = q
			}
			if isApprovalOp(req.Op) {
				return AdministerApproval(ctx, aq, req), nil
			}
			resp := Administer(a, eps, req)
			if req.Op == OpCloudStatus && resp.OK {
				resp.ApprovalServed = served
			}
			return resp, nil
		}
	}
	old, err := NewRelayIssuer(call(false)).Capabilities(t.Context())
	if err != nil || old == nil || old.Approval || old.Ceiling != nil {
		t.Fatalf("older direct server offer %+v %v", old, err)
	}
	r := NewRelayIssuer(call(true))
	caps, err := r.Capabilities(t.Context())
	if err != nil || caps == nil || !caps.Approval || caps.Ceiling == nil || strings.Join(caps.Ceiling.Permissions, ",") != "read,steer" ||
		caps.Ceiling.Resource != hp.DirectResourceWorkgroups || strings.Join(caps.Ceiling.Workgroups, ",") != "wg1,wg2" {
		t.Fatalf("offer %+v ceiling %+v %v", caps, caps.Ceiling, err)
	}
	raw, _ := json.Marshal(caps)
	if !strings.Contains(string(raw), `"approval":true`) || !strings.Contains(string(raw), `"ceiling":{"permissions":["read","steer"],"resource":"workgroups","workgroups":["wg1","wg2"]}`) {
		t.Fatalf("wire %s", raw)
	}
	// An all_sessions ceiling names no workgroups.
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, ResourceScope{})); err != nil {
		t.Fatal(err)
	}
	caps, _ = r.Capabilities(t.Context())
	if raw, _ := json.Marshal(caps.Ceiling); string(raw) != `{"permissions":["read"],"resource":"all_sessions"}` {
		t.Fatalf("all_sessions ceiling %s", raw)
	}
	// Lease/settle through the admin ops.
	q.touch()
	done := make(chan ApprovalVerdict, 1)
	go func() {
		v, _ := q.Verify(context.Background(), hp.DirectApprovalVerify{Approval: testApproval, ServerID: "sid", CodeChallenge: testChallenge})
		done <- v
	}()
	var items []ApprovalItem
	for i := 0; i < 50 && len(items) == 0; i++ {
		items, err = r.LeaseApprovals(t.Context(), 4, 100*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(items) != 1 || items[0].Verify.Approval != testApproval {
		t.Fatalf("leased %+v", items)
	}
	if err := r.SettleApprovals(t.Context(), []ApprovalVerdict{{ID: items[0].ID, ProviderID: testProvider, Result: hp.DirectApprovalResult{Error: hp.DirectApprovalErrUsed}}}); err != nil {
		t.Fatal(err)
	}
	if v := <-done; v.ProviderID != testProvider || v.Result.Error != hp.DirectApprovalErrUsed {
		t.Fatalf("verdict %+v", v)
	}
	// A direct server without approvals refuses the ops.
	if _, err := NewRelayIssuer(call(false)).LeaseApprovals(t.Context(), 1, 0); err == nil {
		t.Fatal("lease accepted by a server that does not serve approvals")
	}
}

// CORS: the allowed origin (configured, or the live Harness webOrigin) may
// preflight and call hello, enroll/cloud, token and the resource API, with
// Authorization allowed and never credentials; any other origin is refused.
func TestCORSForWebOriginOnEnrollAndResourceAPI(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	const web = "https://harness.example.dev"
	var live atomic.Value
	live.Store("")
	s, err := NewServer(Config{Authority: a, Backend: newFake(), Endpoints: []string{testEndpoint}, Logf: func(string, ...any) {},
		Approvals: NewApprovalQueue(), ExtraOrigins: func() []string {
			if v := live.Load().(string); v != "" {
				return []string{v}
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{PathHello, PathEnrollCloud, PathToken, PathSession}
	preflight := func(path, origin string) *http.Response {
		w := do(s, http.MethodOptions, path, "", "", "Origin", origin, "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "authorization")
		return w.Result()
	}
	for _, p := range paths {
		if resp := preflight(p, web); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s allowed before webOrigin is known: %d", p, resp.StatusCode)
		}
	}
	live.Store(web)
	for _, p := range paths {
		resp := preflight(p, web)
		h := resp.Header
		if resp.StatusCode != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != web ||
			!strings.Contains(h.Get("Access-Control-Allow-Headers"), "Authorization") || h.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("%s preflight: %d %v", p, resp.StatusCode, h)
		}
		if resp := preflight(p, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s foreign origin: %d", p, resp.StatusCode)
		}
	}
	// A non-canonical live value is ignored.
	live.Store(web + "/")
	if resp := preflight(PathHello, web+"/"); resp.StatusCode != http.StatusForbidden {
		t.Fatal("non-canonical live origin allowed")
	}
}
