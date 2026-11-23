package direct

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The token and claim endpoints classify enrollment refusals with a
// host_refused reason (ticket_used, ticket_expired, other), and every
// exchange is recorded and logged without its code, verifier or challenge.
func TestEnrollExchangeReasonsAndRecord(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := enabledAuthority(t, c)
	var mu sync.Mutex
	var logs []string
	post := func(path string, v url.Values) (int, OAuthError) {
		// A fresh server per request keeps the per-source failure limit out
		// of the way; the record lives on the authority.
		s, err := NewServer(Config{Authority: a, Backend: newFake(), Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
			WatchPoll: 20 * time.Millisecond, Revalidate: time.Hour, Keepalive: time.Hour,
			Logf: func(f string, v ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, v...)); mu.Unlock() }})
		if err != nil {
			t.Fatal(err)
		}
		w := do(s, http.MethodPost, path, "", form(v), "Content-Type", formCT)
		var e OAuthError
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return w.Code, e
	}
	claimOf := func(code, verifier string) url.Values {
		return url.Values{"code": {code}, "code_challenge": {S256Challenge(verifier)}, "code_challenge_method": {CodeChallengeMethodS256}}
	}
	redeemOf := func(code, verifier string) url.Values {
		return url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Pixel"}, "client_kind": {ClientWeb}, "code_verifier": {verifier}}
	}

	if act := a.EnrollActivity(); act.Last != nil || act.Count != 0 || act.Since != c.t.Unix() {
		t.Fatalf("fresh activity %+v", act)
	}

	// Claimed by another device: ticket_used.
	t1, _, _ := a.IssueCloudCode(ticketReq("k1"))
	if st, _ := post(PathClaim, claimOf(t1, testVerifier)); st != http.StatusOK {
		t.Fatalf("claim %d", st)
	}
	if st, e := post(PathClaim, claimOf(t1, otherVerifier)); st != http.StatusBadRequest || e.Error != OAuthInvalidGrant || e.Reason != ReasonTicketUsed {
		t.Fatalf("second device: %d %+v", st, e)
	}
	// Redeemed, then redeemed again: ticket_used.
	if st, e := post(PathToken, redeemOf(t1, testVerifier)); st != http.StatusOK {
		t.Fatalf("redeem: %d %+v", st, e)
	}
	if act := a.EnrollActivity(); act.Last == nil || !act.Last.OK || act.Last.Step != EnrollStepRedeem || act.Last.ClientKind != ClientWeb {
		t.Fatalf("after redeem %+v", act.Last)
	}
	if _, e := post(PathToken, redeemOf(t1, testVerifier)); e.Reason != ReasonTicketUsed {
		t.Fatalf("reuse: %+v", e)
	}
	if _, e := post(PathClaim, claimOf(t1, testVerifier)); e.Reason != ReasonTicketUsed {
		t.Fatalf("claim after redeem: %+v", e)
	}

	// Expired ticket and code: ticket_expired.
	t2, _, _ := a.IssueCloudCode(ticketReq("k2"))
	k3 := issueReq("k3")
	code3, _, _ := a.IssueCloudCode(k3)
	c.add(CloudCodeTTL + time.Second)
	if _, e := post(PathClaim, claimOf(t2, testVerifier)); e.Reason != ReasonTicketExpired {
		t.Fatalf("expired claim: %+v", e)
	}
	if _, e := post(PathToken, redeemOf(code3, testVerifier)); e.Reason != ReasonTicketExpired {
		t.Fatalf("expired redeem: %+v", e)
	}

	// Policy replaced after issuance: the code is void, reported as expired.
	code4, _, _ := a.IssueCloudCode(issueReq("k4"))
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1)); err != nil {
		t.Fatal(err)
	}
	if _, e := post(PathToken, redeemOf(code4, testVerifier)); e.Reason != ReasonTicketExpired {
		t.Fatalf("stale policy: %+v", e)
	}

	// Unknown code and wrong verifier: other.
	if _, e := post(PathClaim, claimOf("not-a-real-ticket", testVerifier)); e.Reason != ReasonOther {
		t.Fatalf("unknown ticket: %+v", e)
	}
	code5, _, _ := a.IssueCloudCode(issueReq("k5"))
	if _, e := post(PathToken, redeemOf(code5, otherVerifier)); e.Reason != ReasonOther {
		t.Fatalf("wrong verifier: %+v", e)
	}
	// Malformed: invalid_request with no reason on the wire, noted as other.
	if st, e := post(PathClaim, url.Values{"code": {"x"}}); st != http.StatusBadRequest || e.Error != OAuthInvalidRequest || e.Reason != "" {
		t.Fatalf("malformed claim: %d %+v", st, e)
	}
	act := a.EnrollActivity()
	if act.Last == nil || act.Last.OK || act.Last.Step != EnrollStepClaim || act.Last.Reason != ReasonOther || act.Last.Error != OAuthInvalidRequest {
		t.Fatalf("last %+v", act.Last)
	}

	// Refresh requests are not enrollment exchanges: not recorded, no reason.
	before := a.EnrollActivity().Count
	if _, e := post(PathToken, url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {"nope"}}); e.Error != OAuthInvalidGrant || e.Reason != "" {
		t.Fatalf("refresh refusal %+v", e)
	}
	if a.EnrollActivity().Count != before {
		t.Fatal("refresh recorded as an enrollment exchange")
	}

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "\n")
	raw, _ := json.Marshal(a.EnrollActivity())
	for _, secret := range []string{t1, t2, code3, code5, testVerifier, otherVerifier, testChallenge} {
		if strings.Contains(joined, secret) || strings.Contains(string(raw), secret) {
			t.Fatalf("log or record leaks a secret:\n%s\n%s", joined, raw)
		}
	}
	if !strings.Contains(joined, "direct: enrollment ") || !strings.Contains(joined, "claim refused: invalid_grant, reason ticket_used") {
		t.Fatalf("logs:\n%s", joined)
	}
}

// A browser on the tailnet enrolls directly: the Harness web origin (an
// allowed origin) may preflight and call hello, claim and token, and any
// other origin is refused before routing.
func TestEnrollEndpointsCORSForAllowedOriginOnly(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	s := newTestServer(t, a, newFake())
	for _, path := range []string{PathHello, PathClaim, PathToken} {
		w := do(s, http.MethodOptions, path, "", "", "Origin", testOrigin, "Access-Control-Request-Method", "POST",
			"Access-Control-Request-Private-Network", "true")
		h := w.Header()
		if w.Code != http.StatusNoContent || h.Get("Access-Control-Allow-Origin") != testOrigin ||
			!strings.Contains(h.Get("Access-Control-Allow-Methods"), "POST") || h.Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatalf("%s preflight: %d %v", path, w.Code, h)
		}
		if w := do(s, http.MethodOptions, path, "", "", "Origin", "https://evil.example", "Access-Control-Request-Method", "POST"); w.Code != http.StatusForbidden ||
			w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s foreign preflight: %d", path, w.Code)
		}
	}
	// The full ticket flow from the allowed origin: claim then redeem.
	code, _, err := a.IssueCloudCode(ticketReq("cors"))
	if err != nil {
		t.Fatal(err)
	}
	claim := url.Values{"code": {code}, "code_challenge": {testChallenge}, "code_challenge_method": {CodeChallengeMethodS256}}
	if w := do(s, http.MethodPost, PathClaim, "", form(claim), "Content-Type", formCT, "Origin", testOrigin); w.Code != http.StatusOK ||
		w.Header().Get("Access-Control-Allow-Origin") != testOrigin {
		t.Fatalf("claim from browser: %d %s", w.Code, w.Body)
	}
	redeem := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Browser"}, "client_kind": {ClientWeb}, "code_verifier": {testVerifier}}
	if w := do(s, http.MethodPost, PathToken, "", form(redeem), "Content-Type", formCT, "Origin", testOrigin); w.Code != http.StatusOK ||
		w.Header().Get("Access-Control-Allow-Origin") != testOrigin {
		t.Fatalf("redeem from browser: %d %s", w.Code, w.Body)
	}
	if w := do(s, http.MethodPost, PathClaim, "", form(claim), "Content-Type", formCT, "Origin", "https://evil.example"); w.Code != http.StatusForbidden {
		t.Fatalf("foreign claim: %d", w.Code)
	}
}

// A cloud request naming no permissions gets read, plus steer only when the
// host's ceiling includes steer ("Allow steer" defaults on). Nothing else is
// implied, and explicit permissions are unchanged.
func TestCloudIssueDefaultPermissions(t *testing.T) {
	for _, tc := range []struct {
		ceiling []string
		want    string
	}{
		{[]string{PermRead}, "read"},
		{[]string{PermRead, PermSteer}, "read,steer"},
		{[]string{PermRead, PermSteer, PermRespond, PermManage}, "read,steer"},
		{[]string{PermRead, PermManage}, "read"},
	} {
		a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
		if _, err := a.EnableCloud(cloudPolicyReq(tc.ceiling, allSessions)); err != nil {
			t.Fatal(err)
		}
		req := issueReq("k")
		req.Permissions = nil
		code, _, err := a.IssueCloudCode(req)
		if err != nil {
			t.Fatalf("%v: %v", tc.ceiling, err)
		}
		if got := strings.Join(a.state.Codes[hashToken(code)].Perms, ","); got != tc.want {
			t.Errorf("ceiling %v: perms %s, want %s", tc.ceiling, got, tc.want)
		}
	}
}

// Issuance refusals carry their host_refused reason, in the attempt record
// and in the relay result Harness receives.
func TestCloudRefusalReasons(t *testing.T) {
	for code, want := range map[string]string{
		CloudErrScope: ReasonCeiling, CloudErrIssueExpired: ReasonTicketExpired,
		CloudErrOwnerMismatch: ReasonOther, CloudErrDisabled: ReasonOther, CloudErrUnavailable: ReasonOther,
	} {
		if got := CloudReason(code); got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
	a, _ := enabledAuthority(t, nil) // ceiling read,steer on wg1, wg2
	eps := []string{"https://amux.test:8443"}
	over := issueReq("over")
	over.Resource = allSessions
	if _, at := AdministerCloudIssue(a, eps, AdminRequest{Op: OpIssueCloudCode, Issue: &over}); at.Code != CloudErrScope || at.Reason != ReasonCeiling {
		t.Fatalf("attempt %+v", at)
	}
	relay := NewRelayIssuer(func(_ context.Context, req AdminRequest) (AdminResponse, error) {
		return Administer(a, eps, req), nil
	})
	r := relay.Enroll(t.Context(), testProvider, hp.DirectEnrollRequest{IssueKey: "over2", Requester: testOwner,
		CodeChallenge: testChallenge, CodeChallengeMethod: hp.DirectEnrollMethodS256, Permissions: []string{PermRead},
		Resource: hp.DirectResource{Kind: hp.DirectResourceAllSessions}})
	if r.Error != hp.DirectEnrollErrScope || r.Reason != hp.DirectReasonCeiling {
		t.Fatalf("relay result %+v", r)
	}
	ok := relay.Enroll(t.Context(), testProvider, hp.DirectEnrollRequest{IssueKey: "fine", Requester: testOwner,
		CodeChallenge: testChallenge, CodeChallengeMethod: hp.DirectEnrollMethodS256})
	if ok.Error != "" || ok.Reason != "" || ok.Bundle == "" {
		t.Fatalf("defaulted request: %+v", ok)
	}
}
