package daemon

import (
	"context"
	"errors"
	"sync"

	"amux/internal/sessionrpc"
)

var errSessionResponseCapacity = errors.New("daemon session response capacity exceeded")

type sessionResponseBudget struct {
	mu sync.Mutex

	maxFiles    int
	maxBytes    int64
	maxReceipts int

	files    int
	bytes    int64
	receipts int
	leases   map[sessionResponseKey]*sessionResponseLease
}

type sessionResponseKey struct {
	subjectID string
	requestID string
}

type sessionResponseLease struct {
	budget *sessionResponseBudget
	key    sessionResponseKey
	state  sessionResponseLeaseState
}

type sessionResponseLeaseState struct {
	reservedBytes int64
	receipt       bool
	committed     bool
	released      bool
}

func newSessionResponseBudget() *sessionResponseBudget {
	return newSessionResponseBudgetWithLimits(
		sessionrpc.MaxResponseFiles,
		sessionrpc.MaxResponseBytes,
		sessionrpc.MaxPendingReceipts,
	)
}

func newSessionResponseBudgetWithLimits(maxFiles int, maxBytes int64, maxReceipts int) *sessionResponseBudget {
	return &sessionResponseBudget{
		maxFiles: maxFiles, maxBytes: maxBytes, maxReceipts: maxReceipts,
		leases: make(map[sessionResponseKey]*sessionResponseLease),
	}
}

func (b *sessionResponseBudget) ReserveResponse(ctx context.Context, request sessionrpc.ResponseReservation) (sessionrpc.ResponseLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.SubjectID == "" || request.RequestID == "" || request.MaxBytes <= 0 {
		return nil, errSessionResponseCapacity
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionResponseKey{subjectID: request.SubjectID, requestID: request.RequestID}
	if lease := b.leases[key]; lease != nil {
		if request.Existing {
			return lease, nil
		}
		return nil, errSessionResponseCapacity
	}
	if b.files >= b.maxFiles || request.MaxBytes > b.maxBytes-b.bytes ||
		(request.ReceiptSlot && b.receipts >= b.maxReceipts) {
		return nil, errSessionResponseCapacity
	}

	lease := &sessionResponseLease{
		budget: b,
		key:    key,
		state: sessionResponseLeaseState{
			reservedBytes: request.MaxBytes,
			receipt:       request.ReceiptSlot,
		},
	}
	b.leases[key] = lease
	b.files++
	b.bytes += request.MaxBytes
	if request.ReceiptSlot {
		b.receipts++
	}
	return lease, nil
}

func (l *sessionResponseLease) Commit(actualBytes int64, receiptPending bool) {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released || state.committed {
		return
	}
	if actualBytes > 0 && actualBytes < state.reservedBytes {
		b.bytes -= state.reservedBytes - actualBytes
		state.reservedBytes = actualBytes
	}
	state.committed = true
	if state.receipt && !receiptPending {
		b.receipts--
		state.receipt = false
	}
	l.state = state
}

func (l *sessionResponseLease) ReleaseReceipt() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released || !state.receipt {
		return
	}
	b.receipts--
	state.receipt = false
	l.state = state
}

func (l *sessionResponseLease) Release() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released {
		return
	}
	state.released = true
	b.files--
	b.bytes -= state.reservedBytes
	if state.receipt {
		b.receipts--
		state.receipt = false
	}
	delete(b.leases, l.key)
	l.state = state
}
