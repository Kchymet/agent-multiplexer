package provider

import (
	"context"
	"strings"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// runtimeevents.go implements the opt-in "runtime-events" feature (docs/remote-
// provider-sessions.md §4): for a published session the provider tails the local
// runtime's on-disk session record and streams structured transcript events over
// the dialed connection. It is strictly read-only — there is no input counterpart
// — and additive to "sessions" (it streams events for sessions "sessions"
// publishes). Never bridges a pane/terminal.

// onRuntimeEventsSubscribe starts (once per session) a pump that streams the
// session's structured events, resuming from AfterSeq. Ignored when the feature
// isn't active (lenient forward-compat) or the session is already subscribed. The
// pump runs under the session's rtCtx so it stops on connection teardown.
func (p *Provider) onRuntimeEventsSubscribe(s *session, m harnessproto.MuxMsg) {
	if !p.runtimeEventsActive() || !validRuntimeSessionID(m.SessionID) {
		return
	}

	// Membership and resolver admission share opMu. The opening gets its own
	// completion barrier: removal cancels and waits for it without blocking work
	// for retained targets, so the daemon's untracked-conversation fallback is
	// never reachable through an ID whose grant disappeared mid-open.
	s.opMu.Lock()
	if _, ok := s.published[m.SessionID]; !ok || s.rtOpening[m.SessionID] != nil || s.rtSubs[m.SessionID] != nil {
		s.opMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.rtCtx)
	opening := &runtimeOpening{cancel: cancel, done: make(chan struct{})}
	s.rtOpening[m.SessionID] = opening
	s.opMu.Unlock()
	// The producer runs under its own child context so a pump that replaces
	// it (session id reuse) can stop it without ending the subscription.
	prodCtx, prodCancel := context.WithCancel(ctx)
	batches, ok := p.cfg.RuntimeEventStream(prodCtx, m.SessionID, m.AfterSeq)
	close(opening.done)
	s.opMu.Lock()
	active := s.rtOpening[m.SessionID] == opening
	if active {
		delete(s.rtOpening, m.SessionID)
	}
	if !active || !ok || ctx.Err() != nil {
		s.opMu.Unlock()
		prodCancel()
		cancel()
		return
	}
	sub := &runtimeSubscription{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.rtSubs[m.SessionID] = sub
	s.rtWG.Add(1)
	s.opMu.Unlock()
	go func() {
		defer s.rtWG.Done()
		p.pumpRuntimeEvents(s, m.SessionID, sub, batches, prodCancel)
	}()
}

// validRuntimeSessionID rejects path/URI-shaped aliases even if a malformed
// inventory ever contains one. Runtime identifiers are opaque single
// components; the peer never selects a host path for the local resolver.
func validRuntimeSessionID(id string) bool {
	return id != "" && id != "." && id != ".." &&
		!strings.ContainsAny(id, "/\\\\\x00") && !strings.Contains(id, "://")
}

// pumpRuntimeEvents forwards event batches for one session as runtime-events
// frames until the source channel closes (rtCtx cancelled) or a write fails
// (which tears the session down, mirroring the pane sender and publish loop).
func (p *Provider) pumpRuntimeEvents(s *session, sessionID string, sub *runtimeSubscription, batches <-chan harnessproto.RuntimeEventBatch, stopProducer context.CancelFunc) {
	defer func() { stopProducer() }()
	defer func() {
		s.opMu.Lock()
		if s.rtSubs[sessionID] == sub {
			delete(s.rtSubs, sessionID)
		}
		s.opMu.Unlock()
		sub.cancel()
		close(sub.done)
	}()
	// The stream is bound to the session incarnation it was opened for, and
	// every batch re-checks it before emitting:
	//   - lookup failed (unknown): the batch is emitted WITHOUT a label (never
	//     relabelled, never dropped) and the subscription continues;
	//   - confirmed equal: the batch carries the bound label;
	//   - confirmed different (the id was reused): queued batches of the old
	//     record are dropped and the stream is re-opened from the start of the
	//     new record under the new label, so the orchestrator's subscription
	//     stays live with no resubscribe needed.
	// A stream whose incarnation was unknown when it opened is never labelled.
	bound, boundOK := p.sessionIncarnation(sub.ctx, sessionID)
	for {
		select {
		case <-sub.ctx.Done():
			return
		case b, ok := <-batches:
			if !ok {
				return
			}
			if len(b.Events) == 0 {
				continue
			}
			inc := ""
			if cur, ok := p.sessionIncarnation(sub.ctx, sessionID); ok && boundOK {
				if cur != bound {
					// Confirmed reuse: stop the old record's producer (its
					// queued batches are dropped, never relabelled) and re-open
					// from the start of the new record.
					p.cfg.Logf("runtime-events: session %s was reused; re-opening its stream", sessionID)
					stopProducer()
					next, cancelNext, ok := p.reopenRuntimeStream(s, sessionID, sub)
					if !ok {
						return
					}
					batches, bound, stopProducer = next, cur, cancelNext
					continue
				}
				inc = bound
			}
			// Revalidate immediately before every emitted batch and hold the admission
			// lock through the write. Revocation deletes/cancels the subscription, then
			// waits for its done barrier; no queued batch can cross it.
			s.opMu.Lock()
			_, published := s.published[sessionID]
			active := s.rtSubs[sessionID] == sub
			if !published || !active {
				s.opMu.Unlock()
				return
			}
			err := s.hc.WriteHarness(harnessproto.HarnessMsg{
				Type:        harnessproto.HRuntimeEvents,
				SessionID:   sessionID,
				Runtime:     b.Runtime,
				Incarnation: inc,
				Seq:         b.Seq,
				Events:      b.Events,
			})
			s.opMu.Unlock()
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}

// runtimeReopenBackoff bounds the retry delay of a failed re-open.
var runtimeReopenBackoff = struct{ min, max time.Duration }{100 * time.Millisecond, 5 * time.Second}

// reopenRuntimeStream re-opens sessionID's stream from the start under a new
// child producer context, retrying a failed open with bounded backoff for as
// long as the session is still published and this subscription is current
// (so the orchestrator's subscription never silently goes dead). ok=false
// only when the subscription ended.
func (p *Provider) reopenRuntimeStream(s *session, sessionID string, sub *runtimeSubscription) (<-chan harnessproto.RuntimeEventBatch, context.CancelFunc, bool) {
	delay := runtimeReopenBackoff.min
	for {
		s.opMu.Lock()
		_, published := s.published[sessionID]
		current := s.rtSubs[sessionID] == sub
		s.opMu.Unlock()
		if !published || !current || sub.ctx.Err() != nil {
			return nil, nil, false
		}
		prodCtx, prodCancel := context.WithCancel(sub.ctx)
		if next, ok := p.cfg.RuntimeEventStream(prodCtx, sessionID, 0); ok {
			return next, prodCancel, true
		}
		prodCancel()
		select {
		case <-sub.ctx.Done():
			return nil, nil, false
		case <-time.After(delay):
		}
		delay = min(delay*2, runtimeReopenBackoff.max)
	}
}

// maxRuntimeHistoryInFlight bounds concurrent runtime-events-history reads per
// connection; a request beyond it is answered "busy" rather than queued.
const maxRuntimeHistoryInFlight = 4

// runtimeHistoryActive reports whether "runtime-events-history" is active: it
// rides on "runtime-events" and needs a history source.
func (p *Provider) runtimeHistoryActive() bool {
	return p.runtimeEventsActive() && p.cfg.RuntimeEventHistory != nil
}

// onRuntimeEventsHistory answers one runtime-events-history request with exactly
// one runtime-events-page (§4.7). Like a subscription it is admitted only for a
// published session and registered as revocable work: removing the session
// cancels the read and waits for it, and the reply is revalidated under the
// admission lock, so no page of an unpublished session is ever written. The read
// runs off the read loop so a cold index build never stalls pane traffic.
func (p *Provider) onRuntimeEventsHistory(s *session, m harnessproto.MuxMsg) {
	if !p.runtimeHistoryActive() {
		return
	}
	reply := harnessproto.HarnessMsg{Type: harnessproto.HRuntimeEventsPage, ReqID: m.ReqID, SessionID: m.SessionID}
	fail := func(msg string) {
		reply.Error = msg
		s.opMu.Lock()
		err := s.hc.WriteHarness(reply)
		s.opMu.Unlock()
		if err != nil {
			s.cancel()
		}
	}
	if !validRuntimeSessionID(m.SessionID) || m.BeforeSeq < 0 || m.Limit < 0 {
		fail("invalid request")
		return
	}
	s.opMu.Lock()
	if _, ok := s.published[m.SessionID]; !ok {
		s.opMu.Unlock()
		fail(errSessionNotPublished.Error())
		return
	}
	if s.rtHistory >= maxRuntimeHistoryInFlight {
		s.opMu.Unlock()
		fail("busy")
		return
	}
	s.rtHistory++
	ctx, cancel := context.WithCancel(s.rtCtx)
	s.nextOpID++
	opID := s.nextOpID
	call := &sessionActionCall{target: m.SessionID, cancel: cancel, done: make(chan struct{})}
	s.actions[opID] = call
	s.rtWG.Add(1)
	s.opMu.Unlock()

	go func() {
		defer s.rtWG.Done()
		page, ok := p.cfg.RuntimeEventHistory(ctx, m.SessionID, m.BeforeSeq, m.Limit)
		inc := ""
		if ok {
			inc, _ = p.sessionIncarnation(ctx, m.SessionID) // "" when unknown
		}
		cancelled := ctx.Err() != nil
		cancel()
		close(call.done)
		s.opMu.Lock()
		delete(s.actions, opID)
		s.rtHistory--
		_, published := s.published[m.SessionID]
		switch {
		case !published || cancelled:
			reply.Error = errSessionNotPublished.Error()
		case !ok:
			reply.Error = "no runtime record"
		default:
			reply.OK = true
			reply.Runtime, reply.Seq, reply.HeadSeq, reply.HasMore, reply.Events = page.Runtime, page.Seq, page.Head, page.HasMore, page.Events
			reply.Incarnation = inc
		}
		err := s.hc.WriteHarness(reply)
		s.opMu.Unlock()
		if err != nil {
			s.cancel()
		}
	}()
}

// sessionIncarnation is the session's store incarnation; ok=false when it is
// unknown (no lookup configured, or the lookup failed).
func (p *Provider) sessionIncarnation(ctx context.Context, sessionID string) (string, bool) {
	if p.cfg.SessionIncarnation == nil {
		return "", false
	}
	return p.cfg.SessionIncarnation(ctx, sessionID)
}
