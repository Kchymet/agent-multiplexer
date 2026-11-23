package daemon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"amux/internal/sessionrpc"
)

func reserveClass(budget *sessionResponseBudget, subject, request string, p sessionrpc.Priority) (sessionrpc.ResponseLease, error) {
	return budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: subject, RequestID: request, MaxBytes: 1024, ReceiptSlot: true,
		Priority: p, Verb: p.String() + "-verb",
	})
}

// One session can never take more than its own share, so it cannot starve
// another session (2026-11-22: a burst of new agents starved every session).
func TestProductionBudgetIsolatesSessions(t *testing.T) {
	budget := newSessionResponseBudget()
	admitted := 0
	for i := 0; i < 4*sessionrpc.MaxResponseFiles; i++ {
		if _, err := reserveClass(budget, "noisy", fmt.Sprintf("r%d", i), sessionrpc.PriorityControl); err == nil {
			admitted++
		}
	}
	if admitted != sessionrpc.MaxResponseFiles {
		t.Fatalf("one session admitted %d responses, want its ceiling %d", admitted, sessionrpc.MaxResponseFiles)
	}
	if _, err := reserveClass(budget, "quiet", "r0", sessionrpc.PriorityTelemetry); err != nil {
		t.Fatalf("another session's first report refused while one session is saturated: %v", err)
	}
}

// Telemetry is shed first: once a session's telemetry share is used, its
// credential reads and control actions are still admitted.
func TestProductionBudgetReservesCapacityForControl(t *testing.T) {
	budget := newSessionResponseBudget()
	telemetry := 0
	for i := 0; i < sessionrpc.MaxResponseFiles; i++ {
		if _, err := reserveClass(budget, "agent", fmt.Sprintf("t%d", i), sessionrpc.PriorityTelemetry); err == nil {
			telemetry++
		}
	}
	if want := subjectTelemetryShare.of(sessionrpc.MaxResponseFiles); telemetry != want {
		t.Fatalf("telemetry admitted %d, want its share %d", telemetry, want)
	}
	for i := 0; i < 16; i++ {
		if _, err := reserveClass(budget, "agent", fmt.Sprintf("c%d", i), sessionrpc.PriorityControl); err != nil {
			t.Fatalf("control call %d refused behind telemetry: %v", i, err)
		}
	}
	snap := budget.snapshot()
	if snap.Rejected == 0 || snap.RejectedByPriority["telemetry"] == 0 || snap.RejectedByPriority["control"] != 0 ||
		snap.RejectedBySession["agent"] == 0 || snap.RejectedByVerb["telemetry-verb"] == 0 {
		t.Fatalf("rejection counters = %+v", snap)
	}
	if snap.ByPriority["telemetry"] != telemetry || snap.ByPriority["control"] != 16 {
		t.Fatalf("live leases by class = %+v", snap.ByPriority)
	}
}

// Daemon-wide, telemetry from many sessions stops at half the ceiling, and
// control stays admissible past the normal share.
func TestProductionBudgetGlobalTiers(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(16, 1<<30, 16)
	budget.share = productionResponseShares
	var telemetry, normal int
	for i := 0; i < 16; i++ {
		if _, err := reserveClass(budget, fmt.Sprintf("s%d", i), "t", sessionrpc.PriorityTelemetry); err == nil {
			telemetry++
		}
	}
	for i := 0; i < 16; i++ {
		if _, err := reserveClass(budget, fmt.Sprintf("s%d", i), "n", sessionrpc.PriorityNormal); err == nil {
			normal++
		}
	}
	if telemetry != 8 || normal != 6 {
		t.Fatalf("telemetry=%d normal=%d, want 8 and 6 (shares 1/2 and 7/8 of 16)", telemetry, normal)
	}
	for i := 0; i < 2; i++ {
		if _, err := reserveClass(budget, fmt.Sprintf("s%d", i), "c", sessionrpc.PriorityControl); err != nil {
			t.Fatalf("control refused from its reserve: %v", err)
		}
	}
	if _, err := reserveClass(budget, "s9", "c", sessionrpc.PriorityControl); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("full budget admitted control: %v", err)
	}
}

// Releasing every lease returns every counter to zero, per class and session.
func TestProductionBudgetReleaseRestoresAccounting(t *testing.T) {
	budget := newSessionResponseBudget()
	var leases []sessionrpc.ResponseLease
	for i, p := range []sessionrpc.Priority{sessionrpc.PriorityTelemetry, sessionrpc.PriorityNormal, sessionrpc.PriorityControl} {
		lease, err := reserveClass(budget, "a", fmt.Sprintf("r%d", i), p)
		if err != nil {
			t.Fatal(err)
		}
		lease.Commit(100, i == 2)
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		lease.Release()
		lease.Release()
	}
	snap := budget.snapshot()
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 || len(budget.subjects) != 0 || len(snap.ByPriority) != 0 {
		t.Fatalf("leaked accounting: files=%d bytes=%d receipts=%d subjects=%d byClass=%v",
			budget.files, budget.bytes, budget.receipts, len(budget.subjects), snap.ByPriority)
	}
}

func TestSessionCallPriority(t *testing.T) {
	cases := []struct {
		call sessionrpc.Call
		want sessionrpc.Priority
	}{
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "query", Verb: "credential-read"}, sessionrpc.PriorityControl},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "query", Verb: "central-credential"}, sessionrpc.PriorityControl},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "action", Verb: "steer"}, sessionrpc.PriorityControl},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "action", Verb: "add-agent"}, sessionrpc.PriorityControl},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "action", Verb: "report-activity"}, sessionrpc.PriorityTelemetry},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "action", Verb: "report-model"}, sessionrpc.PriorityTelemetry},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "query", Verb: "report-context"}, sessionrpc.PriorityTelemetry},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "action", Verb: "report-permission-request"}, sessionrpc.PriorityNormal},
		{sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: "query", Verb: "snapshot"}, sessionrpc.PriorityNormal},
	}
	for _, c := range cases {
		if got := sessionCallPriority(c.call); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.call.Route, c.call.Verb, got, c.want)
		}
	}
}
