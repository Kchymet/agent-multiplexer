package daemon

import (
	"context"
	"errors"
	"testing"

	"amux/internal/sessionrpc"
)

func reserveTestResponse(t *testing.T, budget *sessionResponseBudget, subject, request string, bytes int64, receipt, existing bool) sessionrpc.ResponseLease {
	t.Helper()
	lease, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: subject, RequestID: request, MaxBytes: bytes,
		ReceiptSlot: receipt, Existing: existing,
	})
	if err != nil {
		t.Fatalf("reserve %s/%s: %v", subject, request, err)
	}
	return lease
}

func TestSessionResponseBudgetIsSharedAcrossSubjects(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(2, 100, 2)
	first := reserveTestResponse(t, budget, "one", "request-one", 60, true, false)

	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "two", RequestID: "request-two", MaxBytes: 41,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate byte admission error = %v", err)
	}
	first.Commit(20, true)
	second := reserveTestResponse(t, budget, "two", "request-two", 70, true, false)
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "three", RequestID: "request-three", MaxBytes: 1,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate file admission error = %v", err)
	}

	first.ReleaseReceipt()
	second.ReleaseReceipt()
	first.Release()
	second.Release()
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 {
		t.Fatalf("budget not fully released: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
}

func TestSessionResponseBudgetReceiptAndExistingLeaseLifecycle(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(3, 100, 1)
	lease := reserveTestResponse(t, budget, "one", "request-one", 50, true, false)
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "two", RequestID: "request-two", MaxBytes: 10, ReceiptSlot: true,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate receipt admission error = %v", err)
	}
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "one", RequestID: "request-one", MaxBytes: 50,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("duplicate new reservation error = %v", err)
	}

	lease.Commit(25, true)
	reopened := reserveTestResponse(t, budget, "one", "request-one", 25, false, true)
	if reopened != lease {
		t.Fatal("existing response did not adopt the retained lease")
	}
	reopened.Commit(25, false)
	reopened.ReleaseReceipt()
	reopened.ReleaseReceipt()
	reopened.Release()
	reopened.Release()
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 {
		t.Fatalf("idempotent lifecycle leaked capacity: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
}

func TestSessionResponseBudgetRejectsCancelledOrInvalidReservation(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.ReserveResponse(ctx, sessionrpc.ResponseReservation{
		SubjectID: "one", RequestID: "request-one", MaxBytes: 1,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reservation error = %v", err)
	}
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("invalid reservation error = %v", err)
	}
}
