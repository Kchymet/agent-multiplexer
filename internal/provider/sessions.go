package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"amux/internal/agent"
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
// the snapshot bytes to compare against next time. A poll error immediately
// suspends the current grants; a write error cancels the session.
func (p *Provider) publishOnce(ctx context.Context, s *session, seq int64, last []byte) (int64, []byte) {
	sess, err := p.cfg.Sessions(ctx)
	if err != nil {
		// Inventory is the authority source, not merely display data. A failed poll
		// cannot retain stale grants indefinitely: suspend everything immediately,
		// cancel live runtime subscriptions, and force the next successful poll to
		// publish before restoring any target.
		s.revokeAllPublished()
		return seq, nil
	}
	if sess == nil {
		sess = []core.Session{}
	}
	next := publishedSessions(sess)
	b, err := json.Marshal(sess)
	if err != nil || bytes.Equal(b, last) {
		return seq, last
	}
	seq++
	if werr := s.writePublishedSnapshot(next, harnessproto.HarnessMsg{
		Type: harnessproto.HSessions, Seq: seq, Sessions: sess,
	}); werr != nil {
		s.cancel()
		return seq, last
	}
	return seq, b
}

// publishedSessions returns the exact non-empty IDs in a successful snapshot.
// Runtime path validation is stricter at subscription time; the inventory map
// also gates lifecycle targets whose IDs need not be filesystem components.
func publishedSessions(sessions []core.Session) map[string]core.Session {
	out := make(map[string]core.Session, len(sessions))
	for _, row := range sessions {
		if row.ID != "" {
			out[row.ID] = row
		}
	}
	return out
}

// writePublishedSnapshot revokes removed IDs and waits only for work admitted
// against those IDs. Retained actions and streams continue across ordinary
// metadata churn. The final write+install holds opMu, so removals precede bytes
// becoming observable and additions are usable as soon as the peer can respond.
func (s *session) writePublishedSnapshot(next map[string]core.Session, msg harnessproto.HarnessMsg) error {
	waits := s.revokeMissingPublished(next)
	waitForRevokedWork(waits)

	s.opMu.Lock()
	if err := s.rtCtx.Err(); err != nil {
		s.opMu.Unlock()
		return err
	}
	if err := s.hc.WriteHarness(msg); err != nil {
		waits = s.revokeAllPublishedLocked()
		s.opMu.Unlock()
		waitForRevokedWork(waits)
		return err
	}
	s.published = next
	s.opMu.Unlock()
	return nil
}

func (s *session) revokeMissingPublished(next map[string]core.Session) []<-chan struct{} {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	removed := map[string]bool{}
	for id := range s.published {
		if _, ok := next[id]; !ok {
			removed[id] = true
			delete(s.published, id)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	var waits []<-chan struct{}
	for _, call := range s.actions {
		if removed[call.target] {
			call.cancel()
			waits = append(waits, call.done)
		}
	}
	for id, opening := range s.rtOpening {
		if removed[id] {
			opening.cancel()
			waits = append(waits, opening.done)
			delete(s.rtOpening, id)
		}
	}
	for id, sub := range s.rtSubs {
		if removed[id] {
			sub.cancel()
			waits = append(waits, sub.done)
			delete(s.rtSubs, id)
		}
	}
	return waits
}

func (s *session) revokeAllPublished() {
	s.opMu.Lock()
	waits := s.revokeAllPublishedLocked()
	s.opMu.Unlock()
	waitForRevokedWork(waits)
}

func (s *session) revokeAllPublishedLocked() []<-chan struct{} {
	s.published = map[string]core.Session{}
	var waits []<-chan struct{}
	for _, call := range s.actions {
		call.cancel()
		waits = append(waits, call.done)
	}
	for id, opening := range s.rtOpening {
		opening.cancel()
		waits = append(waits, opening.done)
		delete(s.rtOpening, id)
	}
	for id, sub := range s.rtSubs {
		sub.cancel()
		waits = append(waits, sub.done)
		delete(s.rtSubs, id)
	}
	return waits
}

func waitForRevokedWork(waits []<-chan struct{}) {
	for _, done := range waits {
		<-done
	}
}

// handleSessionAction executes one verb and replies with a session-result
// correlated by ReqID. It runs inline on the read loop, which serializes verbs;
// the store operations are quick and a steering verb's PTY write is queued by
// the engine rather than performed here, so neither stalls the loop.
//
// Every relayed verb leaves one line in the log. Until it did, the provider
// recorded only dial/register/disconnect, so "did the prompt I sent from the web
// actually reach this machine?" had no answer here at all — the question you ask
// first when a relay looks stuck, and the one the journal could not answer.
func (p *Provider) handleSessionAction(s *session, m harnessproto.MuxMsg) {
	if !p.publishing() {
		return
	}
	started := time.Now()
	res := harnessproto.HarnessMsg{Type: harnessproto.HSessionResult, ReqID: m.ReqID}
	newID, err := p.applyAuthorizedSessionAction(s, m)
	if err != nil {
		res.Error = err.Error()
	} else {
		res.OK, res.NewID = true, newID
		// A steering verb is taken on, not completed: say "accepted" so the
		// orchestrator waits for runtime-events or the next inventory snapshot
		// instead of assuming the turn is done (spec §2). A `prompt` to a stopped
		// agent is answered before the runtime has even started, so this is the
		// only thing the reply can honestly claim.
		res.Result = harnessproto.ResultApplied
		if harnessproto.SteeringVerbs[m.Action] {
			res.Result, res.Accepted = harnessproto.ResultAccepted, true
		}
	}
	// Logged before the reply is written, not after: the line's whole job is to
	// say when this machine was done with the verb, and one that landed after the
	// answer went out could not be compared against an upstream timeout. The
	// elapsed time is the daemon's handling, which is what that comparison needs —
	// the socket write is the orchestrator's side of the story.
	p.logSessionAction(m, res, time.Since(started))
	if werr := s.hc.WriteHarness(res); werr != nil {
		s.cancel()
	}
}

var errSessionNotPublished = errors.New("session is not currently published")

// applyAuthorizedSessionAction binds every targeted verb to the connection's
// current published inventory. Admission registers a cancellable call and its
// completion barrier under opMu. Removing that target cancels and waits for the
// call without disturbing retained targets; once revocation completes no queued
// action can use the old ID.
// New-workgroup is the one creation verb with no existing target; enabling the
// session-control hook deliberately grants it. A newly created ID is not usable
// until a later successful snapshot publishes it.
func (p *Provider) applyAuthorizedSessionAction(s *session, m harnessproto.MuxMsg) (string, error) {
	if _, ok := sessionActionFor(m); !ok {
		return p.applySessionAction(s.rtCtx, m)
	}
	if m.Action != harnessproto.VerbNewWorkgroup && m.ID == "" {
		return "", errSessionNotPublished
	}

	s.opMu.Lock()
	if m.Action != harnessproto.VerbNewWorkgroup {
		if _, ok := s.published[m.ID]; !ok {
			s.opMu.Unlock()
			return "", errSessionNotPublished
		}
	}
	ctx, cancel := context.WithCancel(s.rtCtx)
	s.nextOpID++
	opID := s.nextOpID
	call := &sessionActionCall{target: m.ID, cancel: cancel, done: make(chan struct{})}
	s.actions[opID] = call
	s.opMu.Unlock()

	newID, err := p.applySessionAction(ctx, m)
	cancel()
	close(call.done)
	s.opMu.Lock()
	delete(s.actions, opID)
	s.opMu.Unlock()
	return newID, err
}

// logSessionAction records one relayed verb: which session, which verb, how it
// came out, and how long it took. Elapsed is the number that makes the line
// worth having — it separates "the daemon never answered" from "the daemon
// answered slowly", which is the whole diagnosis of a relay that times out
// upstream.
//
// It deliberately logs no `fields`. Those carry the prompt text and a permission
// prompt's reason: the user's own words, which have no business in a machine's
// log just because they passed through it. The verb and the session id are what
// an operator needs, and they are not the user's content.
func (p *Provider) logSessionAction(m harnessproto.MuxMsg, res harnessproto.HarnessMsg, elapsed time.Duration) {
	id := m.ID
	if id == "" {
		id = "-" // creation verbs (new-workgroup) name no existing session
	}
	took := elapsed.Round(time.Millisecond)
	if !res.OK {
		p.cfg.Logf("session-action %s id=%s failed in %s: %s", m.Action, id, took, res.Error)
		return
	}
	disposition := res.Result
	if disposition == "" {
		disposition = harnessproto.ResultApplied
	}
	if res.NewID != "" {
		p.cfg.Logf("session-action %s id=%s %s in %s (created %s)",
			m.Action, id, disposition, took, res.NewID)
		return
	}
	p.cfg.Logf("session-action %s id=%s %s in %s", m.Action, id, disposition, took)
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
func (p *Provider) applySessionAction(ctx context.Context, m harnessproto.MuxMsg) (string, error) {
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
	if m.Action == harnessproto.VerbNewWorkgroup || m.Action == harnessproto.VerbAddAgent {
		kind := agent.Canonical(m.Fields["agent"])
		identity := m.Fields["identity_mode"]
		if identity == "" {
			identity = harnessproto.IdentityMachine
		}
		if identity != harnessproto.IdentityMachine || (p.cfg.Execution != nil && !p.cfg.Execution.Supports(kind, identity)) {
			return "", fmt.Errorf("unsupported harness/identity: %s/%s", kind, identity)
		}
	}
	return p.cfg.ApplyAction(ctx, act)
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
