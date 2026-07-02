package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"amux/internal/core"
	"amux/internal/harnessproto"
)

// This file implements the opt-in "sessions" feature (docs/remote-provider-sessions.md):
// the provider publishes its session inventory upstream on the dialed connection
// and accepts a small, fixed set of lifecycle verbs back. It carries no pane or
// terminal access — those remain the separate compute-provider path (spawn/input/
// resize ⇄ output/exit); this feature never bridges them.

// sessionPollInterval is the debounce cadence for inventory publishing.
func (p *Provider) sessionPollInterval() time.Duration {
	if p.cfg.SessionPollInterval > 0 {
		return p.cfg.SessionPollInterval
	}
	return time.Second
}

// onSessionsSubscribe records that the orchestrator asked to receive inventory,
// releasing the publish loop. Ignored (a no-op) when the feature isn't active,
// so a stray subscribe from a misbehaving peer never publishes anything.
func (p *Provider) onSessionsSubscribe(s *session) {
	if !p.publishing() {
		return
	}
	s.subOnce.Do(func() { close(s.subscribe) })
}

// publishLoop pushes full-inventory snapshots to a subscribed orchestrator: an
// initial one on subscribe, then on change (marshal-and-compare, debounced at
// sessionPollInterval). It runs per connection with fresh state, so a reconnect
// re-publishes a complete snapshot from seq 1. A write error tears the session
// down (mirrors the pane sender and heartbeat).
func (p *Provider) publishLoop(ctx context.Context, s *session) {
	select {
	case <-s.done:
		return
	case <-ctx.Done():
		return
	case <-s.subscribe:
	}

	var (
		seq  int64
		last []byte
	)
	seq, last = p.publishOnce(ctx, s, seq, last)

	t := time.NewTicker(p.sessionPollInterval())
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			seq, last = p.publishOnce(ctx, s, seq, last)
		}
	}
}

// publishOnce polls the inventory and, if it changed since last, pushes a new
// sessions frame with the next seq. It returns the (possibly advanced) seq and
// the snapshot bytes to compare against next time. A poll error keeps the prior
// state (nothing is pushed); a write error cancels the session.
func (p *Provider) publishOnce(ctx context.Context, s *session, seq int64, last []byte) (int64, []byte) {
	sess, err := p.cfg.Sessions(ctx)
	if err != nil {
		return seq, last
	}
	if sess == nil {
		sess = []core.Session{}
	}
	b, err := json.Marshal(sess)
	if err != nil || bytes.Equal(b, last) {
		return seq, last
	}
	seq++
	if werr := s.hc.WriteHarness(harnessproto.HarnessMsg{
		Type: harnessproto.HSessions, Seq: seq, Sessions: sess,
	}); werr != nil {
		s.cancel()
		return seq, last
	}
	return seq, b
}

// handleSessionAction executes one lifecycle verb and replies with a
// session-result correlated by ReqID. It runs inline on the read loop, which
// serializes verbs; the store operations themselves are quick.
func (p *Provider) handleSessionAction(s *session, m harnessproto.MuxMsg) {
	if !p.publishing() {
		return
	}
	res := harnessproto.HarnessMsg{Type: harnessproto.HSessionResult, ReqID: m.ReqID}
	newID, err := p.applySessionAction(m)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK, res.NewID = true, newID
	}
	if werr := s.hc.WriteHarness(res); werr != nil {
		s.cancel()
	}
}

// errUnsupported rejects a verb outside the accepted set (spec §3). Its string
// is the exact wire error the spec mandates.
var errUnsupported = errors.New(harnessproto.ErrUnsupported)

// applySessionAction validates and executes a verb. The daemon is authoritative:
// unknown/excluded verbs (including any pane/terminal verb) are rejected with
// "unsupported", and read-only mode rejects every verb. Accepted verbs map to the
// daemon's own lifecycle core.Actions and run through ApplyAction (wsops).
func (p *Provider) applySessionAction(m harnessproto.MuxMsg) (string, error) {
	act, ok := sessionActionFor(m)
	if !ok {
		return "", errUnsupported
	}
	if p.cfg.ReadOnlySessions || p.cfg.ApplyAction == nil {
		return "", errors.New("read-only: session verbs are disabled")
	}
	return p.cfg.ApplyAction(context.Background(), act)
}

// sessionActionFor maps an accepted wire verb to the equivalent daemon
// core.Action, or reports ok=false for anything outside the fixed set. archive/
// unarchive normalize to the daemon's explicit set-archived so the result is
// deterministic (not a toggle).
func sessionActionFor(m harnessproto.MuxMsg) (core.Action, bool) {
	switch m.Action {
	case harnessproto.VerbNewWorkgroup:
		return core.Action{Action: "new-workgroup", Fields: m.Fields}, true
	case harnessproto.VerbAddAgent:
		return core.Action{Action: "add-agent", ID: m.ID, Fields: m.Fields}, true
	case harnessproto.VerbRename:
		return core.Action{Action: "rename", ID: m.ID, Fields: m.Fields}, true
	case harnessproto.VerbArchive:
		return core.Action{Action: "set-archived", ID: m.ID, Fields: map[string]string{"archived": "true"}}, true
	case harnessproto.VerbUnarchive:
		return core.Action{Action: "set-archived", ID: m.ID, Fields: map[string]string{"archived": "false"}}, true
	case harnessproto.VerbStart:
		return core.Action{Action: core.ActionStart, ID: m.ID}, true
	default:
		return core.Action{}, false
	}
}
