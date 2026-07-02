package provider

import (
	"amux/internal/harnessproto"
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
	if !p.runtimeEventsActive() || m.SessionID == "" {
		return
	}
	s.rtMu.Lock()
	if s.rtSubs[m.SessionID] {
		s.rtMu.Unlock()
		return
	}
	s.rtSubs[m.SessionID] = true
	s.rtMu.Unlock()

	batches, ok := p.cfg.RuntimeEventStream(s.rtCtx, m.SessionID, m.AfterSeq)
	if !ok {
		// No structured record for this session: advertise-but-emit-nothing. Clear
		// the dedupe marker so a later subscribe (once a record exists) can retry.
		s.rtMu.Lock()
		delete(s.rtSubs, m.SessionID)
		s.rtMu.Unlock()
		return
	}

	s.rtWG.Add(1)
	go func() {
		defer s.rtWG.Done()
		p.pumpRuntimeEvents(s, m.SessionID, batches)
	}()
}

// pumpRuntimeEvents forwards event batches for one session as runtime-events
// frames until the source channel closes (rtCtx cancelled) or a write fails
// (which tears the session down, mirroring the pane sender and publish loop).
func (p *Provider) pumpRuntimeEvents(s *session, sessionID string, batches <-chan harnessproto.RuntimeEventBatch) {
	for {
		select {
		case <-s.rtCtx.Done():
			return
		case b, ok := <-batches:
			if !ok {
				return
			}
			if len(b.Events) == 0 {
				continue
			}
			if err := s.hc.WriteHarness(harnessproto.HarnessMsg{
				Type:      harnessproto.HRuntimeEvents,
				SessionID: sessionID,
				Seq:       b.Seq,
				Events:    b.Events,
			}); err != nil {
				s.cancel()
				return
			}
		}
	}
}
