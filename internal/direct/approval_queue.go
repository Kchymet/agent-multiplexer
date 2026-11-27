package direct

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Tailnet cloud approvals (docs/direct-tailnet-enroll.md, direct-approval-v1).
// A client posts a Harness approval to POST /direct/v1/enroll/cloud; the host
// must have Harness verify and consume it. Only the provider (`amux cloud`)
// holds the Harness connection, and it runs in another process, so the direct
// server parks each verify here and the provider long-polls for them over the
// host-only admin socket (OpApprovalLease), sends direct-approval-verify on its
// connection, and settles Harness's answer back (OpApprovalSettle). Nothing is
// written to disk or logged: approvals live in memory for at most
// ApprovalVerifyTimeout.

// Approval bridge bounds.
const (
	// ApprovalVerifyTimeout bounds one verify, end to end (the contract's
	// 10 s reply bound).
	ApprovalVerifyTimeout = 10 * time.Second
	// MaxApprovalsInFlight bounds verifies waiting at once (the contract's
	// 4 in flight per connection).
	MaxApprovalsInFlight = 4
	// approvalPollFresh is how recently the provider must have polled for the
	// host to consider Harness reachable at all; with no recent poll a verify
	// fails at once with orchestrator_unreachable instead of waiting.
	approvalPollFresh = 30 * time.Second
	// approvalLeaseMaxWait keeps a lease long-poll inside the admin deadline.
	approvalLeaseMaxWait = 12 * time.Second
)

// approvalWait is ApprovalVerifyTimeout; tests shorten it.
var approvalWaitTimeout = ApprovalVerifyTimeout

var (
	errApprovalBusy        = errors.New("too many approvals in flight")
	errApprovalUnreachable = errors.New("the orchestrator did not answer")
)

// ApprovalItem is one parked verify, as leased to the provider.
type ApprovalItem struct {
	ID     string                  `json:"id"`
	Verify hp.DirectApprovalVerify `json:"verify"`
}

// ApprovalVerdict is the provider's answer for one leased item: Harness's
// result and the provider id of the connection that verified it (the host
// checks it against its provider pin).
type ApprovalVerdict struct {
	ID         string                  `json:"id"`
	ProviderID string                  `json:"provider_id"`
	Result     hp.DirectApprovalResult `json:"result"`
}

// ApprovalAdminRequest is AdminRequest.Approval.
type ApprovalAdminRequest struct {
	Max        int               `json:"max,omitempty"`
	WaitMillis int64             `json:"wait_ms,omitempty"`
	Verdicts   []ApprovalVerdict `json:"verdicts,omitempty"`
}

// ApprovalAdminResponse is AdminResponse.Approval.
type ApprovalAdminResponse struct {
	Items []ApprovalItem `json:"items,omitempty"`
}

type approvalWait struct {
	item   ApprovalItem
	leased bool
	done   chan ApprovalVerdict
}

// ApprovalQueue parks verifies for the provider.
type ApprovalQueue struct {
	mu       sync.Mutex
	pending  map[string]*approvalWait
	order    []string
	signal   chan struct{}
	lastPoll time.Time
	now      func() time.Time
}

// NewApprovalQueue makes an empty queue.
func NewApprovalQueue() *ApprovalQueue {
	return &ApprovalQueue{pending: map[string]*approvalWait{}, signal: make(chan struct{}), now: time.Now}
}

// Verify parks one verify until the provider settles it, the timeout passes
// or ctx ends. It fails fast when no provider has polled recently or too many
// verifies are already waiting.
func (q *ApprovalQueue) Verify(ctx context.Context, v hp.DirectApprovalVerify) (ApprovalVerdict, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ApprovalVerdict{}, err
	}
	w := &approvalWait{item: ApprovalItem{ID: base64.RawURLEncoding.EncodeToString(b), Verify: v}, done: make(chan ApprovalVerdict, 1)}
	q.mu.Lock()
	switch {
	case q.lastPoll.IsZero() || q.now().Sub(q.lastPoll) > approvalPollFresh:
		q.mu.Unlock()
		return ApprovalVerdict{}, errApprovalUnreachable
	case len(q.pending) >= MaxApprovalsInFlight:
		q.mu.Unlock()
		return ApprovalVerdict{}, errApprovalBusy
	}
	q.pending[w.item.ID] = w
	q.order = append(q.order, w.item.ID)
	close(q.signal)
	q.signal = make(chan struct{})
	q.mu.Unlock()
	defer q.drop(w.item.ID)
	t := time.NewTimer(approvalWaitTimeout)
	defer t.Stop()
	select {
	case v := <-w.done:
		return v, nil
	case <-t.C:
	case <-ctx.Done():
	}
	return ApprovalVerdict{}, errApprovalUnreachable
}

func (q *ApprovalQueue) drop(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, id)
	for i, x := range q.order {
		if x == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
}

// Lease hands up to max unleased verifies to the provider, waiting up to wait
// for one to arrive. Every call counts as the provider being present.
func (q *ApprovalQueue) Lease(ctx context.Context, max int, wait time.Duration) []ApprovalItem {
	if max <= 0 || max > MaxApprovalsInFlight {
		max = MaxApprovalsInFlight
	}
	if wait > approvalLeaseMaxWait {
		wait = approvalLeaseMaxWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		q.lastPoll = q.now()
		var out []ApprovalItem
		for _, id := range q.order {
			if w := q.pending[id]; w != nil && !w.leased && len(out) < max {
				w.leased = true
				out = append(out, w.item)
			}
		}
		sig := q.signal
		q.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-sig:
		case <-deadline.C:
			q.touch()
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

func (q *ApprovalQueue) touch() {
	q.mu.Lock()
	q.lastPoll = q.now()
	q.mu.Unlock()
}

// Settle delivers verdicts to their waiting verifies; unknown ids (timed out
// meanwhile) are ignored.
func (q *ApprovalQueue) Settle(verdicts []ApprovalVerdict) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.lastPoll = q.now()
	for _, v := range verdicts {
		if w := q.pending[v.ID]; w != nil && w.leased {
			select {
			case w.done <- v:
			default:
			}
		}
	}
}

// AdministerApproval executes OpApprovalLease or OpApprovalSettle. q may be
// nil (approvals not served): both are refused.
func AdministerApproval(ctx context.Context, q *ApprovalQueue, req AdminRequest) AdminResponse {
	if q == nil {
		return AdminResponse{Error: "cloud approvals are not served by this direct server"}
	}
	ar := req.Approval
	if ar == nil {
		ar = &ApprovalAdminRequest{}
	}
	switch req.Op {
	case OpApprovalLease:
		items := q.Lease(ctx, ar.Max, time.Duration(ar.WaitMillis)*time.Millisecond)
		return AdminResponse{OK: true, Approval: &ApprovalAdminResponse{Items: items}}
	case OpApprovalSettle:
		q.Settle(ar.Verdicts)
		return AdminResponse{OK: true}
	}
	return AdminResponse{Error: "unknown approval operation"}
}

func isApprovalOp(op string) bool { return op == OpApprovalLease || op == OpApprovalSettle }
