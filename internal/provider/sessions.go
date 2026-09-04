package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
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

// handleSessionAction executes one verb and replies with a session-result
// correlated by ReqID. It runs inline on the read loop, which serializes verbs;
// the store operations are quick and a steering verb's PTY write is queued by
// the engine rather than performed here, so neither stalls the loop.
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
		// A steering verb is delivered to a running agent, not completed: say
		// "accepted" so the orchestrator waits for runtime-events or the next
		// inventory snapshot instead of assuming the turn is done (spec §2).
		res.Result = harnessproto.ResultApplied
		if harnessproto.SteeringVerbs[m.Action] {
			res.Result = harnessproto.ResultAccepted
		}
	}
	if werr := s.hc.WriteHarness(res); werr != nil {
		s.cancel()
	}
}

// errUnsupported rejects a verb outside the accepted set (spec §3). Its string
// is the exact wire error the spec mandates.
var errUnsupported = errors.New(harnessproto.ErrUnsupported)

// errUnsupportedVerb rejects a verb that *is* in harnessproto.SessionVerbs but
// this daemon has no implementation for (spec §3.2) — the steering verbs, until
// their handler lands. The two errors are deliberately distinct: "unsupported"
// tells the orchestrator the verb is never valid, "unsupported verb" tells it
// this daemon is simply older than the verb, so it degrades its UI instead of
// treating the connection as broken.
var errUnsupportedVerb = errors.New(harnessproto.ErrUnsupportedVerb)

// applySessionAction validates and executes a verb. The daemon is authoritative:
// unknown/excluded verbs (including any pane/terminal verb) are rejected with
// "unsupported", an accepted verb this daemon does not implement with
// "unsupported verb", and read-only mode rejects every verb — steering verbs
// (spec §3.1) exactly as much as lifecycle ones. Accepted verbs map to the
// daemon's own lifecycle core.Actions and run through ApplyAction (wsops).
func (p *Provider) applySessionAction(m harnessproto.MuxMsg) (string, error) {
	act, ok := sessionActionFor(m)
	if !ok {
		if harnessproto.SessionVerbs[m.Action] {
			return "", errUnsupportedVerb
		}
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
// deterministic (not a toggle); the four steering verbs (spec §3.1) all become
// one core.ActionSteer whose Fields name which, so the daemon has a single
// engine-only entry point for "drive the agent inside this session".
func sessionActionFor(m harnessproto.MuxMsg) (core.Action, bool) {
	switch m.Action {
	case harnessproto.VerbNewWorkgroup:
		return core.Action{Action: core.ActionNewWorkgroup, Fields: m.Fields}, true
	case harnessproto.VerbAddAgent:
		return core.Action{Action: core.ActionAddAgent, ID: m.ID, Fields: m.Fields}, true
	case harnessproto.VerbRename:
		return core.Action{Action: core.ActionRename, ID: m.ID, Fields: m.Fields}, true
	case harnessproto.VerbArchive:
		return core.Action{Action: core.ActionSetArchived, ID: m.ID, Fields: map[string]string{"archived": "true"}}, true
	case harnessproto.VerbUnarchive:
		return core.Action{Action: core.ActionSetArchived, ID: m.ID, Fields: map[string]string{"archived": "false"}}, true
	case harnessproto.VerbStart:
		return core.Action{Action: core.ActionStart, ID: m.ID}, true
	case harnessproto.VerbPrompt, harnessproto.VerbInterject,
		harnessproto.VerbStop, harnessproto.VerbPermission:
		return core.Action{Action: core.ActionSteer, ID: m.ID, Fields: steerFields(m)}, true
	default:
		return core.Action{}, false
	}
}

// steerFields normalizes a steering session-action's wire fields into the
// core.Action fields the daemon's steer handler reads. The wire and the daemon
// deliberately spell the keys the same way (harnessproto.Field* / core.Steer*),
// so this only has to name the verb and copy what that verb carries — an unknown
// key on the wire is dropped rather than passed through to the runtime.
func steerFields(m harnessproto.MuxMsg) map[string]string {
	f := map[string]string{core.SteerVerb: m.Action}
	for _, k := range []string{
		harnessproto.FieldText, harnessproto.FieldRequestID,
		harnessproto.FieldDecision, harnessproto.FieldReason,
	} {
		if v, ok := m.Fields[k]; ok {
			f[k] = v
		}
	}
	return f
}
