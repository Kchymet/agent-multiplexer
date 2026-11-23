package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"sync"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Tailnet cloud approvals (direct-approval-v1, docs/direct-tailnet-enroll.md).
// A client posts a Harness approval directly to this host's direct server;
// the direct server parks a verify, and the provider (which holds the Harness
// connection) leases it, sends one direct-approval-verify frame, and settles
// Harness's answer back. The approval is never logged, queued durably or
// replayed: it lives in memory for one verify.

// ApprovalItem is one parked verify leased from the direct server.
type ApprovalItem struct {
	ID     string
	Verify hp.DirectApprovalVerify
}

// ApprovalVerdict is Harness's answer for one item, with the provider id of
// the connection that verified it.
type ApprovalVerdict struct {
	ID         string
	ProviderID string
	Result     hp.DirectApprovalResult
}

// DirectApprover is the provider's bridge to the direct server's approval
// queue (production: direct.RelayIssuer through an adapter).
type DirectApprover interface {
	LeaseApprovals(ctx context.Context, max int, wait time.Duration) ([]ApprovalItem, error)
	SettleApprovals(ctx context.Context, verdicts []ApprovalVerdict) error
}

// Approval bridge bounds: one reply within directApprovalTimeout (the
// contract's 10 s), at most maxDirectApprovalsInFlight per connection.
var (
	directApprovalTimeout   = 10 * time.Second
	directApprovalLeaseWait = 10 * time.Second
	directApprovalRetry     = 2 * time.Second
)

const maxDirectApprovalsInFlight = 4

// approvalState is per-connection.
type approvalState struct {
	mu       sync.Mutex
	pending  map[string]chan hp.DirectApprovalResult
	inFlight int
}

// approvalAdvertised reports whether this registration offers
// direct-approval-v1.
func approvalAdvertised(c *hp.DirectCapabilities) bool { return c != nil && c.Approval }

// runDirectApproval leases parked verifies and answers each until the
// session ends.
func (p *Provider) runDirectApproval(s *session) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for s.rtCtx.Err() == nil {
		s.approval.mu.Lock()
		free := maxDirectApprovalsInFlight - s.approval.inFlight
		s.approval.mu.Unlock()
		if free <= 0 {
			free = 1 // lease anyway: an item beyond the bound is answered unavailable at once
		}
		items, err := p.cfg.DirectApprove.LeaseApprovals(s.rtCtx, free, directApprovalLeaseWait)
		if err != nil {
			select {
			case <-s.done:
				return
			case <-time.After(directApprovalRetry):
			}
			continue
		}
		for _, it := range items {
			wg.Add(1)
			go func(it ApprovalItem) {
				defer wg.Done()
				start := time.Now()
				r := p.verifyApproval(s, it.Verify)
				outcome := "ok"
				if r.Error != "" {
					outcome = r.Error
				}
				// Outcome and timing only: never the approval or challenge.
				p.cfg.Logf("direct-approval verify: %s in %dms", outcome, time.Since(start).Milliseconds())
				ctx, cancel := context.WithTimeout(context.WithoutCancel(s.rtCtx), directDiscoverTimeout)
				_ = p.cfg.DirectApprove.SettleApprovals(ctx, []ApprovalVerdict{{ID: it.ID, ProviderID: s.providerID, Result: r}})
				cancel()
			}(it)
		}
	}
}

func newApprovalReqID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "da-" + base64.RawURLEncoding.EncodeToString(b)
}

// verifyApproval sends one direct-approval-verify frame and waits for its
// result. Busy, write failure, timeout and teardown all answer unavailable,
// so the client retries with the same approval and challenge.
func (p *Provider) verifyApproval(s *session, v hp.DirectApprovalVerify) hp.DirectApprovalResult {
	unavailable := hp.DirectApprovalResult{Error: hp.DirectApprovalErrUnavailable}
	if !hp.ValidDirectApproval(v.Approval) || v.ServerID == "" || v.CodeChallenge == "" {
		return hp.DirectApprovalResult{Error: hp.DirectApprovalErrMismatch}
	}
	as := &s.approval
	as.mu.Lock()
	if as.inFlight >= maxDirectApprovalsInFlight {
		as.mu.Unlock()
		return unavailable
	}
	as.inFlight++
	id := newApprovalReqID()
	ch := make(chan hp.DirectApprovalResult, 1)
	if as.pending == nil {
		as.pending = map[string]chan hp.DirectApprovalResult{}
	}
	as.pending[id] = ch
	as.mu.Unlock()
	defer func() {
		as.mu.Lock()
		delete(as.pending, id)
		as.inFlight--
		as.mu.Unlock()
	}()
	if s.rtCtx.Err() != nil || s.hc.WriteHarness(hp.HarnessMsg{Type: hp.HDirectApprovalVerify, ReqID: id, DirectApproval: &v}) != nil {
		return unavailable
	}
	t := time.NewTimer(directApprovalTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r
	case <-t.C:
	case <-s.done:
	}
	return unavailable
}

// onDirectApprovalResult hands a result to its waiting verify.
func (p *Provider) onDirectApprovalResult(s *session, m hp.MuxMsg) {
	if m.DirectApprovalResult == nil {
		return
	}
	s.approval.mu.Lock()
	ch := s.approval.pending[m.ReqID]
	s.approval.mu.Unlock()
	if ch != nil {
		select {
		case ch <- *m.DirectApprovalResult:
		default:
		}
	}
}

func sameCeiling(a, b *hp.DirectCeiling) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Resource == b.Resource && slices.Equal(a.Permissions, b.Permissions) && slices.Equal(a.Workgroups, b.Workgroups)
}
