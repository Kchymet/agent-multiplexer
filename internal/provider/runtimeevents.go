package provider

import (
	"context"
	"strings"

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
	batches, ok := p.cfg.RuntimeEventStream(ctx, m.SessionID, m.AfterSeq)
	close(opening.done)
	s.opMu.Lock()
	active := s.rtOpening[m.SessionID] == opening
	if active {
		delete(s.rtOpening, m.SessionID)
	}
	if !active || !ok || ctx.Err() != nil {
		s.opMu.Unlock()
		cancel()
		return
	}
	sub := &runtimeSubscription{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.rtSubs[m.SessionID] = sub
	s.rtWG.Add(1)
	s.opMu.Unlock()
	go func() {
		defer s.rtWG.Done()
		p.pumpRuntimeEvents(s, m.SessionID, sub, batches)
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
func (p *Provider) pumpRuntimeEvents(s *session, sessionID string, sub *runtimeSubscription, batches <-chan harnessproto.RuntimeEventBatch) {
	defer func() {
		s.opMu.Lock()
		if s.rtSubs[sessionID] == sub {
			delete(s.rtSubs, sessionID)
		}
		s.opMu.Unlock()
		sub.cancel()
		close(sub.done)
	}()
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
				Type:      harnessproto.HRuntimeEvents,
				SessionID: sessionID,
				Runtime:   b.Runtime,
				Seq:       b.Seq,
				Events:    b.Events,
			})
			s.opMu.Unlock()
			if err != nil {
				s.cancel()
				return
			}
		}
	}
}
