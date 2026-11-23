package main

import (
	"fmt"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"

	"amux/internal/direct"
	"amux/internal/provider"
)

// directEnrollView answers "did an enrollment ever reach this host?" for amux
// direct status. Three places on this host see enrollment traffic, and each
// records its latest request (time, kind or step, outcome, refusal code and
// reason; never a requester, issue key, code, verifier or token):
//
//   - the direct server's token and claim endpoints, which a client (web or
//     Android, over the tailnet) calls directly to claim a ticket and redeem
//     a code: the request that proves a client reached this host;
//   - the provider, which receives Harness's direct-enroll frame asking for a
//     cloud-issued code or device ticket;
//   - the direct server's issuance, which answers that frame.
//
// So an operator can tell "never reached this host" (the failure is between
// the client and this host: tailnet membership, ACLs, DNS, TLS) from "reached
// this host and was refused here" (and why).
type directEnrollView struct {
	ReachedHost       bool                          `json:"reached_host"`
	ExchangeLast      *direct.EnrollAttempt         `json:"exchange_last,omitempty"`
	ExchangeCount     int                           `json:"exchange_count"`
	ExchangeSince     int64                         `json:"exchange_since,omitempty"`
	ProviderStartedAt *time.Time                    `json:"provider_started_at,omitempty"`
	ProviderLast      *provider.DirectEnrollAttempt `json:"provider_last,omitempty"`
	ProviderCount     int                           `json:"provider_count"`
	ServerLast        *direct.CloudAttempt          `json:"server_last,omitempty"`
	ServerCount       int                           `json:"server_count"`
	ServerSince       int64                         `json:"server_since,omitempty"`
	Note              string                        `json:"note"`
}

// directIssueHints explain a cloud issuance refusal code to the operator.
var directIssueHints = map[string]string{
	hp.DirectEnrollErrDisabled:         "cloud enrollment is off or no direct server answered (amux cloud enroll enable)",
	hp.DirectEnrollErrOwnerMismatch:    "the requesting Harness user is not the owner this host pinned (amux cloud enroll status)",
	hp.DirectEnrollErrProviderMismatch: "this provider is not the one this host pinned (amux cloud enroll status)",
	hp.DirectEnrollErrInvalidRequest:   "Harness sent a malformed request",
	hp.DirectEnrollErrInvalidChallenge: "the client's PKCE challenge was not S256",
	hp.DirectEnrollErrScope:            "the requested permissions or scope exceed this host's cloud ceiling",
	hp.DirectEnrollErrIdemConflict:     "an issue key was reused for a different request",
	hp.DirectEnrollErrIssueExpired:     "the code for this attempt was used or expired; start a new enrollment",
	hp.DirectEnrollErrCapacity:         "too many unredeemed cloud codes; redeem or wait for one to expire",
	hp.DirectEnrollErrRateLimited:      "the hourly cloud enrollment limit is reached",
	hp.DirectEnrollErrBusy:             "the provider had too many enrollment requests in flight",
	hp.DirectEnrollErrUnavailable:      "the direct server did not answer in time or failed (amux device logs)",
}

// directReasonHints explain a direct exchange refusal reason.
var directReasonHints = map[string]string{
	direct.ReasonCeiling:       "the request asks for more than this host allows",
	direct.ReasonTicketExpired: "the code or ticket expired; mint a new one",
	direct.ReasonTicketUsed:    "the code or ticket was already used or claimed by another device; mint a new one",
	direct.ReasonOther:         "see amux device logs",
	// Approval exchanges (POST /direct/v1/enroll/cloud).
	direct.ReasonMalformed:               "the client sent a malformed request",
	direct.ReasonOwnerMismatch:           "the approving Harness user is not the owner this host pinned (amux cloud enroll status)",
	direct.ReasonProviderMismatch:        "the verifying provider is not the one this host pinned (amux cloud enroll status)",
	direct.ReasonApprovalUnknown:         "Harness does not know this approval (voided by a retry?); start over in Harness",
	direct.ReasonApprovalMismatch:        "the approval was issued for another machine, server or device",
	direct.ReasonCloudDisabled:           "cloud enrollment is off on this host (amux cloud enroll enable)",
	direct.ReasonRateLimited:             "too many attempts or codes; wait and retry",
	direct.ReasonOrchestratorUnreachable: "this host could not reach Harness to verify the approval: is amux cloud running and registered?",
	direct.ReasonUnavailable:             "a transient failure; retry with the same approval",
}

func hintFor(code string) string {
	if h, ok := directIssueHints[code]; ok {
		return h
	}
	return "unknown refusal"
}

func directEnrollment(srv *directServerStatus, st provider.Status, live bool) directEnrollView {
	v := directEnrollView{}
	if srv != nil && srv.Cloud != nil {
		v.ServerLast, v.ServerCount, v.ServerSince = srv.Cloud.LastAttempt, srv.Cloud.Attempts, srv.Cloud.AttemptsSince
	}
	if srv != nil && srv.Enroll != nil {
		v.ExchangeLast, v.ExchangeCount, v.ExchangeSince = srv.Enroll.Last, srv.Enroll.Count, srv.Enroll.Since
	}
	if live {
		v.ProviderLast, v.ProviderCount = st.DirectEnrollLast, st.DirectEnrollCount
		if !st.StartedAt.IsZero() {
			started := st.StartedAt
			v.ProviderStartedAt = &started
		}
	}
	v.ReachedHost = v.ExchangeLast != nil || v.ProviderLast != nil || v.ServerLast != nil
	ts := func(unix int64) string { return time.Unix(unix, 0).Local().Format(time.RFC3339) }
	switch {
	case !v.ReachedHost && srv == nil:
		v.Note = "no direct server is answering, so no client can enroll (amux device enable --tailnet)"
	case !v.ReachedHost:
		v.Note = "no enrollment request has reached this host since the direct server started (" + ts(v.ExchangeSince) + "); " +
			"if a client reports it cannot reach this machine, the failure is between that device and this host " +
			"(is the device on the tailnet, and can it reach " + firstEndpoint(srv) + "?)"
		if !live {
			v.Note += "; no provider is running, so Harness cannot issue cloud tickets either"
		}
	case v.ExchangeLast != nil && (v.ProviderLast == nil || v.ExchangeLast.At >= v.ProviderLast.At.Unix()):
		e := v.ExchangeLast
		if e.OK {
			v.Note = fmt.Sprintf("a client reached this host directly and its %s succeeded at %s", e.Step, ts(e.At))
		} else {
			v.Note = fmt.Sprintf("a client reached this host directly at %s and its %s was refused: reason %s (%s) — %s",
				ts(e.At), e.Step, e.Reason, e.Error, directReasonHints[e.Reason])
		}
	case v.ProviderLast != nil:
		p := v.ProviderLast
		if p.Outcome == "issued" {
			v.Note = "Harness's latest cloud enrollment request reached this host and a code was issued at " + p.At.Local().Format(time.RFC3339) +
				"; no client has redeemed it here since"
		} else {
			v.Note = fmt.Sprintf("Harness's latest cloud enrollment request reached this host at %s and was refused here: %s — %s",
				p.At.Local().Format(time.RFC3339), p.Code, hintFor(p.Code))
		}
	default:
		// Only the direct server's issuance saw a request: an earlier provider
		// process relayed it.
		v.Note = "the direct server's latest cloud issuance: " + v.ServerLast.String()
		if v.ServerLast.Outcome == direct.CloudOutcomeRefused {
			v.Note += " — " + hintFor(v.ServerLast.Code)
		}
	}
	return v
}

func firstEndpoint(srv *directServerStatus) string {
	if srv != nil && len(srv.Endpoints) > 0 {
		return srv.Endpoints[0]
	}
	return "its endpoint"
}

func printDirectEnroll(v directEnrollView) {
	fmt.Printf("enroll    %s\n", v.Note)
	if v.ExchangeLast != nil {
		fmt.Printf("          last client exchange: %s (%d since %s)\n", v.ExchangeLast, v.ExchangeCount,
			time.Unix(v.ExchangeSince, 0).Format(time.RFC3339))
	}
	if v.ProviderLast != nil {
		fmt.Printf("          last cloud request at the provider: %s (%d since the provider started)\n", v.ProviderLast, v.ProviderCount)
	}
	if v.ServerLast != nil {
		fmt.Printf("          last cloud issuance at the direct server: %s (%d since %s)\n", v.ServerLast, v.ServerCount,
			time.Unix(v.ServerSince, 0).Format(time.RFC3339))
	}
}
