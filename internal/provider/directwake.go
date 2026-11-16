package provider

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Direct wake-ups (docs/direct-wake-contract.md). The provider forwards only
// opaque handles the host's direct server leases to it, and reports Harness's
// answer back so the host settles each item only on a terminal answer. It
// needs neither "sessions" nor "runtime-events".

// WakeOffer is the host's wake policy as the provider sees it.
type WakeOffer struct {
	ServerID    string
	OwnerPin    string
	ProviderPin string
}

// WakeItem is one leased outbox item (Secret only on activate items).
type WakeItem struct {
	ID, Kind, RouteID, WakeID, GrantRef, Requester, Secret string
}

// WakeSettle is Harness's answer for one leased item.
type WakeSettle struct {
	ID, Status string
}

// DirectWaker is the provider's narrow bridge to the host's direct server
// (production: direct.WakeRelay through an adapter).
type DirectWaker interface {
	Status(ctx context.Context) (*WakeOffer, error)
	Lease(ctx context.Context, max int, wait time.Duration) ([]WakeItem, error)
	Settle(ctx context.Context, results []WakeSettle) error
	Routes(ctx context.Context) ([]hp.DirectWakeRouteRef, error)
}

var (
	directWakeFrameTimeout = 15 * time.Second
	directWakeRouteEvery   = 30 * time.Second
	directWakeLeaseWait    = 10 * time.Second
	directWakeRetry        = 2 * time.Second
)

const maxDirectWakeInFlight = 4

// wakeState is per-connection.
type wakeState struct {
	mu       sync.Mutex
	pending  map[string]chan hp.DirectWakeResult
	inFlight int
}

func (p *Provider) probeDirectWake(ctx context.Context) *WakeOffer {
	if p.cfg.DirectWake == nil {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, directDiscoverTimeout)
	offer, err := p.cfg.DirectWake.Status(dctx)
	cancel()
	if err != nil || offer == nil || offer.ServerID == "" {
		return nil
	}
	// The pins must match this host's registration. Before the first
	// registration of this process they are unknown and checked once
	// registered (runSession); after it, a mismatch means no offer.
	p.stMu.Lock()
	pid, owner := p.status.ProviderID, p.status.OwnerUserID
	p.stMu.Unlock()
	if pid != "" && (offer.ProviderPin != pid || offer.OwnerPin != owner) {
		return nil
	}
	return offer
}

func (p *Provider) discoverDirectWake(ctx context.Context) {
	offer := p.probeDirectWake(ctx)
	p.mu.Lock()
	p.directWake = offer
	p.mu.Unlock()
}

func (p *Provider) directWakeOffer() *WakeOffer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.directWake == nil {
		return nil
	}
	c := *p.directWake
	return &c
}

func sameWakeOffer(a, b *WakeOffer) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func echoed(features []string, f string) bool {
	for _, x := range features {
		if x == f {
			return true
		}
	}
	return false
}

// startDirectWake runs the wake loops for a registered session whose
// orchestrator echoed direct-wake-v1. It returns false (and the caller
// re-registers without the advert) when the host's pins do not match this
// registration.
func (p *Provider) wakePinsMatch(offer *WakeOffer, providerID string, owner *hp.RegisteredOwner) bool {
	return offer != nil && owner != nil && offer.ProviderPin == providerID && offer.OwnerPin == owner.UserID
}

// watchDirectWake keeps the advertised wake offer true for the session's life
// (the same re-register rule as the enroll advert).
func (p *Provider) watchDirectWake(s *session, advertised *WakeOffer) {
	t := time.NewTicker(p.directRecheck)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		now := p.probeDirectWake(s.rtCtx)
		if s.rtCtx.Err() != nil {
			return
		}
		if sameWakeOffer(now, advertised) {
			misses = 0
			continue
		}
		misses++
		if misses < p.directMisses {
			continue
		}
		p.cfg.Logf("direct-wake offer changed; re-registering")
		s.cancel()
		return
	}
}

func newReqID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "dw-" + base64.RawURLEncoding.EncodeToString(b)
}

// sendWake writes one direct-wake* frame and waits for its result. Any
// failure (busy, write error, timeout, teardown) is reported as an error
// result so the host returns the items for retry.
func (p *Provider) sendWake(s *session, typ string, body *hp.DirectWake) hp.DirectWakeResult {
	ws := &s.dwake
	ws.mu.Lock()
	if ws.inFlight >= maxDirectWakeInFlight {
		ws.mu.Unlock()
		return hp.DirectWakeResult{Error: hp.DirectWakeBusy}
	}
	ws.inFlight++
	id := newReqID()
	ch := make(chan hp.DirectWakeResult, 1)
	if ws.pending == nil {
		ws.pending = map[string]chan hp.DirectWakeResult{}
	}
	ws.pending[id] = ch
	ws.mu.Unlock()
	defer func() {
		ws.mu.Lock()
		delete(ws.pending, id)
		ws.inFlight--
		ws.mu.Unlock()
	}()
	if s.rtCtx.Err() != nil || s.hc.WriteHarness(hp.HarnessMsg{Type: typ, ReqID: id, DirectWake: body}) != nil {
		return hp.DirectWakeResult{Error: hp.DirectWakeUnavailable}
	}
	t := time.NewTimer(directWakeFrameTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r
	case <-t.C:
	case <-s.done:
	}
	return hp.DirectWakeResult{Error: hp.DirectWakeUnavailable}
}

// onDirectWakeResult hands a result to its waiting sender.
func (p *Provider) onDirectWakeResult(s *session, m hp.MuxMsg) {
	if m.DirectWakeResult == nil {
		return
	}
	s.dwake.mu.Lock()
	ch := s.dwake.pending[m.ReqID]
	s.dwake.mu.Unlock()
	if ch != nil {
		select {
		case ch <- *m.DirectWakeResult:
		default:
		}
	}
}

// runDirectWake drives route sync and the lease/forward/settle loop until the
// session ends.
func (p *Provider) runDirectWake(s *session, offer *WakeOffer) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.wakeRouteLoop(s, offer) }()
	go func() { defer wg.Done(); p.wakeLeaseLoop(s, offer) }()
	wg.Wait()
}

func (p *Provider) wakeRouteLoop(s *session, offer *WakeOffer) {
	t := time.NewTicker(directWakeRouteEvery)
	defer t.Stop()
	for {
		p.syncRoutes(s, offer)
		select {
		case <-s.done:
			return
		case <-t.C:
		}
	}
}

// syncRoutes sends one complete epoch. A failure to read the snapshot sends
// nothing (an absent epoch never counts as a miss at Harness).
func (p *Provider) syncRoutes(s *session, offer *WakeOffer) {
	ctx, cancel := context.WithTimeout(s.rtCtx, directDiscoverTimeout)
	routes, err := p.cfg.DirectWake.Routes(ctx)
	cancel()
	if err != nil || len(routes) > hp.MaxDirectWakeRouteParts*hp.MaxDirectWakeRoutes {
		return
	}
	epoch := newReqID()
	parts := (len(routes) + hp.MaxDirectWakeRoutes - 1) / hp.MaxDirectWakeRoutes
	if parts == 0 {
		parts = 1
	}
	for i := 0; i < parts; i++ {
		lo, hi := i*hp.MaxDirectWakeRoutes, min((i+1)*hp.MaxDirectWakeRoutes, len(routes))
		body := &hp.DirectWake{ServerID: offer.ServerID, Epoch: epoch, Part: i, Final: i == parts-1, Routes: routes[lo:hi]}
		if r := p.sendWake(s, hp.HDirectWakeRoutes, body); r.Error != "" {
			return
		}
	}
}

// directWakeLeaseMax bounds one lease round: at most this many items, so its
// frames (one per activation, one for all wakes) never exceed
// maxDirectWakeInFlight-1 (a slot stays free for route sync), and the whole
// round is bounded by one directWakeFrameTimeout, well inside the host's
// 30 s lease.
const directWakeLeaseMax = maxDirectWakeInFlight - 1

func (p *Provider) wakeLeaseLoop(s *session, offer *WakeOffer) {
	for s.rtCtx.Err() == nil {
		items, err := p.cfg.DirectWake.Lease(s.rtCtx, directWakeLeaseMax, directWakeLeaseWait)
		if err != nil {
			select {
			case <-s.done:
				return
			case <-time.After(directWakeRetry):
			}
			continue
		}
		if len(items) == 0 {
			continue
		}
		p.forwardWakes(s, offer, items, func(r []WakeSettle) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(s.rtCtx), directDiscoverTimeout)
			_ = p.cfg.DirectWake.Settle(ctx, r)
			cancel()
		})
	}
}

// forwardWakes sends each activation in its own frame and all wakes in one
// frame, concurrently, and settles every item as soon as its own answer
// arrives: an early success is durable even while slower frames are still in
// flight. A frame-level error leaves that frame's items retryable.
func (p *Provider) forwardWakes(s *session, offer *WakeOffer, items []WakeItem, settle func([]WakeSettle)) {
	var wg sync.WaitGroup
	var wakes []WakeItem
	for _, it := range items {
		if it.Kind != "activate" {
			wakes = append(wakes, it)
			continue
		}
		wg.Add(1)
		go func(it WakeItem) {
			defer wg.Done()
			r := p.sendWake(s, hp.HDirectWakeActivate, &hp.DirectWake{
				ServerID: offer.ServerID, RouteID: it.RouteID, ActivationSecret: it.Secret, GrantRef: it.GrantRef, Requester: it.Requester,
			})
			settle([]WakeSettle{{ID: it.ID, Status: statusFor(r, it.RouteID, "")}})
		}(it)
	}
	if len(wakes) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := &hp.DirectWake{ServerID: offer.ServerID}
			for _, it := range wakes {
				body.Wakes = append(body.Wakes, hp.DirectWakeRef{RouteID: it.RouteID, WakeID: it.WakeID, GrantRef: it.GrantRef})
			}
			r := p.sendWake(s, hp.HDirectWake, body)
			out := make([]WakeSettle, 0, len(wakes))
			for _, it := range wakes {
				out = append(out, WakeSettle{ID: it.ID, Status: statusFor(r, it.RouteID, it.WakeID)})
			}
			settle(out)
		}()
	}
	wg.Wait()
}

func statusFor(r hp.DirectWakeResult, routeID, wakeID string) string {
	if r.Error != "" {
		return hp.DirectWakeUnavailable
	}
	for _, it := range r.Results {
		if it.RouteID == routeID && it.WakeID == wakeID {
			switch it.Status {
			case hp.DirectWakeAccepted, hp.DirectWakeNoRecipient, hp.DirectWakeRouteGone,
				hp.DirectWakeRateLimited, hp.DirectWakeBusy:
				return it.Status
			}
			return hp.DirectWakeUnavailable
		}
	}
	return hp.DirectWakeUnavailable
}
