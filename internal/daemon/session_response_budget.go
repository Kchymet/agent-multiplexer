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

// daemonResponseSessions sizes the daemon-wide response budget for this many
// sessions' worth of full per-session queues. Each mailbox still enforces its
// own MaxResponseFiles / MaxResponseBytes (Server.ensureResponseCapacity), so
// on-disk usage stays bounded per session; this budget only bounds aggregate
// amplification across sessions.
//
// It used to be sized for ONE session (128 files, 8 MiB) while being shared by
// every session. Each in-flight request reserves the worst-case envelope
// (~93 KiB) until commit, so only ~87 requests could be in flight across the
// whole daemon. With ~25 agents reporting hooks and status lines (~180 calls a
// minute), a restart herd filled it; deferred clients timed out after 10s;
// their uncollected responses then held slots until MaxResponseAge (60s), so
// the budget never drained and credential-read starved for every agent
// (incident 2026-11-22, daemon livelock).
const daemonResponseSessions = 32

func newSessionResponseBudget() *sessionResponseBudget {
	return newSessionResponseBudgetWithLimits(
		sessionrpc.MaxResponseFiles*daemonResponseSessions,
		sessionrpc.MaxResponseBytes*daemonResponseSessions,
		sessionrpc.MaxPendingReceipts*daemonResponseSessions,
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
	// Existing responses are already durable disk reality from a prior daemon
	// incarnation. Adopt and account them even when the retained set exceeds the
	// current limits; rejecting adoption would wedge startup before cleanup can
	// release anything. Only new amplification is subject to admission limits.
	if !request.Existing && (b.files >= b.maxFiles || request.MaxBytes > b.maxBytes-b.bytes ||
		(request.ReceiptSlot && b.receipts >= b.maxReceipts)) {
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
