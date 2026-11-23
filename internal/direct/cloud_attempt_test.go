package direct

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The default cloud ceiling is every ordinary session; the console stays
// refused in every ceiling.
func TestCloudPolicyCeilingDefaultsToAllSessions(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	st, err := a.EnableCloud(CloudPolicyRequest{Permissions: []string{PermRead}, OwnerPin: testOwner, ProviderPin: testProvider})
	if err != nil {
		t.Fatal(err)
	}
	if st.Resource.Kind != ResourceAllSessions || len(st.Resource.Workgroups) != 0 {
		t.Fatalf("default ceiling %+v", st.Resource)
	}
	for _, id := range []string{ConsoleID, OverseerID} {
		if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{id}})); err == nil {
			t.Fatalf("ceiling naming %s accepted", id)
		}
	}
	// Kind-less workgroups are still malformed, not defaulted.
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, ResourceScope{Workgroups: []string{"wg1"}})); err == nil {
		t.Fatal("kind-less workgroups accepted")
	}
}

// A relayed request that names no resource asks for the host's ceiling.
func TestCloudIssueWithoutResourceTakesCeiling(t *testing.T) {
	a, _ := enabledAuthority(t, nil) // ceiling: workgroups wg1, wg2
	req := issueReq("k-none")
	req.Resource = ResourceScope{}
	code, _, err := a.IssueCloudCode(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := a.state.Codes[hashToken(code)]
	if rec.Resource.Kind != ResourceWorkgroups || strings.Join(rec.Resource.Workgroups, ",") != "wg1,wg2" {
		t.Fatalf("defaulted resource %+v", rec.Resource)
	}
	// The same key and request replays the same code.
	if again, _, err := a.IssueCloudCode(req); err != nil || again != code {
		t.Fatalf("retry: %v", err)
	}

	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, allSessions)); err != nil {
		t.Fatal(err)
	}
	req = issueReq("k-all")
	req.Resource = ResourceScope{}
	code, _, err = a.IssueCloudCode(req)
	if err != nil {
		t.Fatal(err)
	}
	if rec := a.state.Codes[hashToken(code)]; rec.Resource.Kind != ResourceAllSessions {
		t.Fatalf("defaulted resource %+v", rec.Resource)
	}
	// The console is never reachable, defaulted or not.
	req = issueReq("k-console")
	req.Resource = ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{ConsoleID}}
	if _, _, err := a.IssueCloudCode(req); cloudCode(t, err) != CloudErrInvalidRequest {
		t.Fatalf("console: %v", err)
	}
}

// Every relayed issuance that reaches the direct server is recorded (time,
// kind, outcome, refusal code) without its requester, issue key or code, so
// status can tell "never reached this host" from "reached it and failed".
func TestCloudAttemptsRecorded(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := enabledAuthority(t, c)
	eps := []string{"https://amux.test:8443"}

	st := a.CloudStatus()
	if st.LastAttempt != nil || st.Attempts != 0 || st.AttemptsSince != c.t.Unix() {
		t.Fatalf("fresh status %+v", st)
	}

	c.t = c.t.Add(time.Minute)
	bad := issueReq("secret-issue-key-1")
	bad.Requester = "someone_else"
	resp, at := AdministerCloudIssue(a, eps, AdminRequest{Op: OpIssueCloudCode, Issue: &bad})
	if resp.OK || at.Outcome != CloudOutcomeRefused || at.Code != CloudErrOwnerMismatch || at.Kind != CloudKindCode || at.At != c.t.Unix() {
		t.Fatalf("refusal recorded as %+v", at)
	}

	c.t = c.t.Add(time.Minute)
	ticket := issueReq("secret-issue-key-2")
	ticket.Handoff, ticket.CodeChallenge, ticket.CodeChallengeMethod = HandoffDeviceTicket, "", ""
	resp = Administer(a, eps, AdminRequest{Op: OpIssueCloudCode, Issue: &ticket})
	if !resp.OK {
		t.Fatalf("ticket: %+v", resp)
	}
	st = a.CloudStatus()
	if st.Attempts != 2 || st.LastAttempt == nil || *st.LastAttempt != (CloudAttempt{At: c.t.Unix(), Kind: CloudKindDeviceTicket, Outcome: CloudOutcomeIssued}) {
		t.Fatalf("status %+v last %+v", st, st.LastAttempt)
	}
	if s := st.LastAttempt.String(); !strings.Contains(s, "issued (device_ticket)") {
		t.Fatalf("String %q", s)
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{"secret-issue-key", testChallenge, resp.Bundle, "someone_else"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("status leaks %q: %s", secret, raw)
		}
	}

	// No endpoints, and no request: still recorded as reaching the host.
	at = noteThrough(a, nil, &bad)
	if at.Code != CloudErrUnavailable {
		t.Fatalf("no endpoints: %+v", at)
	}
	at = noteThrough(a, eps, nil)
	if at.Code != CloudErrInvalidRequest || at.Kind != CloudKindOther {
		t.Fatalf("no request: %+v", at)
	}

	// Attempts survive disabling (and are reported with no policy at all).
	if _, err := a.DisableCloud("test"); err != nil {
		t.Fatal(err)
	}
	if st := a.CloudStatus(); st.Attempts != 4 || st.LastAttempt == nil {
		t.Fatalf("after disable %+v", st)
	}
	fresh := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	_, at = AdministerCloudIssue(fresh, eps, AdminRequest{Op: OpIssueCloudCode, Issue: &bad})
	if at.Code != CloudErrDisabled || fresh.CloudStatus().LastAttempt == nil {
		t.Fatalf("no policy: %+v", at)
	}
}

func noteThrough(a *Authority, eps []string, req *CloudIssueRequest) CloudAttempt {
	_, at := AdministerCloudIssue(a, eps, AdminRequest{Op: OpIssueCloudCode, Issue: req})
	return at
}
