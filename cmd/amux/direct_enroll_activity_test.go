package main

import (
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"

	"amux/internal/direct"
	"amux/internal/provider"
)

func TestDirectEnrollmentView(t *testing.T) {
	started := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	at := started.Add(time.Hour)
	srv := func(enroll *direct.EnrollAttempt, cloud *direct.CloudAttempt) *directServerStatus {
		s := &directServerStatus{ServerID: "sid", Endpoints: []string{"https://host.example.ts.net:8443"},
			Cloud:  &direct.CloudStatus{Enabled: true, AttemptsSince: started.Unix(), LastAttempt: cloud},
			Enroll: &direct.EnrollActivity{Since: started.Unix(), Last: enroll}}
		if enroll != nil {
			s.Enroll.Count = 1
		}
		if cloud != nil {
			s.Cloud.Attempts = 1
		}
		return s
	}
	live := provider.Status{StartedAt: started}
	for name, tc := range map[string]struct {
		srv     *directServerStatus
		st      provider.Status
		live    bool
		reached bool
		want    string
	}{
		"no server":            {nil, live, true, false, "no direct server is answering"},
		"never reached":        {srv(nil, nil), live, true, false, "can it reach https://host.example.ts.net:8443?"},
		"never, no provider":   {srv(nil, nil), provider.Status{}, false, false, "Harness cannot issue cloud tickets either"},
		"local ticket claimed": {srv(&direct.EnrollAttempt{At: at.Unix(), Step: direct.EnrollStepClaim, OK: true}, nil), provider.Status{}, false, true, "its claim succeeded"},
		"redeem refused": {srv(&direct.EnrollAttempt{At: at.Unix(), Step: direct.EnrollStepRedeem, ClientKind: "web",
			Error: direct.OAuthInvalidGrant, Reason: direct.ReasonTicketExpired}, nil), live, true, true,
			"its redeem was refused: reason ticket_expired (invalid_grant) — the code or ticket expired"},
		"issued, not redeemed": {srv(nil, nil), provider.Status{StartedAt: started, DirectEnrollCount: 1,
			DirectEnrollLast: &provider.DirectEnrollAttempt{At: at, Kind: "pkce_code", Outcome: "issued"}}, true, true, "no client has redeemed it here since"},
		"issuance refused": {srv(nil, nil), provider.Status{StartedAt: started, DirectEnrollCount: 1,
			DirectEnrollLast: &provider.DirectEnrollAttempt{At: at, Kind: "pkce_code", Outcome: "refused", Code: hp.DirectEnrollErrOwnerMismatch}}, true, true,
			"was refused here: owner_mismatch — the requesting Harness user is not the owner"},
		"exchange after issuance": {srv(&direct.EnrollAttempt{At: at.Add(time.Minute).Unix(), Step: direct.EnrollStepRedeem, OK: true}, nil),
			provider.Status{StartedAt: started, DirectEnrollLast: &provider.DirectEnrollAttempt{At: at, Outcome: "issued"}}, true, true, "its redeem succeeded"},
		"earlier provider": {srv(nil, &direct.CloudAttempt{At: at.Unix(), Kind: direct.CloudKindCode, Outcome: direct.CloudOutcomeRefused, Code: direct.CloudErrOwnerMismatch}),
			provider.Status{StartedAt: at.Add(time.Hour)}, true, true, "the direct server's latest cloud issuance"},
	} {
		v := directEnrollment(tc.srv, tc.st, tc.live)
		if v.ReachedHost != tc.reached || !strings.Contains(v.Note, tc.want) {
			t.Errorf("%s: reached=%v note %q", name, v.ReachedHost, v.Note)
		}
	}
	for _, code := range []string{hp.DirectEnrollErrDisabled, hp.DirectEnrollErrOwnerMismatch, hp.DirectEnrollErrProviderMismatch,
		hp.DirectEnrollErrInvalidRequest, hp.DirectEnrollErrInvalidChallenge, hp.DirectEnrollErrScope, hp.DirectEnrollErrIdemConflict,
		hp.DirectEnrollErrIssueExpired, hp.DirectEnrollErrCapacity, hp.DirectEnrollErrRateLimited, hp.DirectEnrollErrBusy, hp.DirectEnrollErrUnavailable} {
		if hintFor(code) == "unknown refusal" {
			t.Errorf("no hint for %s", code)
		}
	}
	for _, r := range []string{direct.ReasonCeiling, direct.ReasonTicketExpired, direct.ReasonTicketUsed, direct.ReasonOther} {
		if directReasonHints[r] == "" {
			t.Errorf("no hint for reason %s", r)
		}
	}
}

// With neither scope flag, cloud enable's ceiling is all ordinary sessions.
func TestDirectResourceFlagsDefaultAllSessions(t *testing.T) {
	res, err := directResourceFlags(false, nil)
	if err != nil || res.Kind != direct.ResourceAllSessions || len(res.Workgroups) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := directResourceFlags(false, []string{"wg1"}); err != nil || res.Kind != direct.ResourceWorkgroups {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := directResourceFlags(true, []string{"wg1"}); err == nil {
		t.Fatal("both accepted")
	}
}
