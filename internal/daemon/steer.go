package daemon

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/runtimeevents"
	"amux/internal/store"
	"amux/internal/wsops"
)

// This file serves core.ActionSteer: driving the agent *inside* a running
// session (docs/remote-provider-sessions.md §3.1). Like ActionStart it is
// engine-only — it changes no store state — so daemon.handle serves it directly
// and it never reaches wsops.
//
// Delivery is by keystroke to the agent pane's PTY, because that is the only
// input an interactive runtime (the Claude Code and Codex TUIs) actually has.
// Which bytes each runtime expects is the agent-kind registry's business
// (agent.Harness.Keys), not a switch here: adding a kind must not mean editing
// the daemon.

// steerStartSettle is how long a just-started runtime is given to paint its
// composer before the text is typed into it. A TUI that hasn't finished booting
// drops keystrokes, so a `prompt` that starts the agent waits this long first.
// It is a heuristic, which is exactly why a steering verb reports "accepted"
// rather than "applied".
const steerStartSettle = 2 * time.Second

// steer delivers one steering verb to a session's agent pane. It returns an
// error the caller surfaces verbatim, so every refusal says why: an unsteerable
// agent kind, a stopped agent, a missing field, an unparseable decision.
//
// It never blocks on the PTY: engine.Instance.InputSequence queues and returns, so
// a child that has stopped draining its input can't stall the connection loop
// that called this.
func (d *Daemon) steer(ctx context.Context, a core.Action) error {
	verb := a.Fields[core.SteerVerb]
	if a.ID == "" {
		return fmt.Errorf("steer: need a session id")
	}
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}

	h, sess, err := d.steerTarget(a.ID)
	if err != nil {
		return err
	}

	// Structured control (AGE-181): a Codex session under the App Server supervisor
	// is steered by JSON-RPC, not keystrokes. Route to the supervisor and skip the
	// entire PTY path. A structured session whose supervisor is not live yet is
	// started by `prompt` alone, mirroring the PTY "prompt starts a stopped agent"
	// rule.
	if d.codex != nil {
		if sup, ok := d.codex.Get(a.ID); ok {
			return d.steerStructured(ctx, a.ID, sup, verb, a.Fields)
		}
		if d.structuredControl(sess) {
			if verb != core.SteerPrompt {
				return fmt.Errorf("agent %s is not running (only %q starts a stopped agent)", a.ID, core.SteerPrompt)
			}
			return d.startStructuredForPrompt(ctx, sess, a.Fields)
		}
	}

	keys := h.Keys()
	if !keys.Steerable() {
		return fmt.Errorf("agent kind %q has no steering keys", agent.Canonical(sess.Agent))
	}
	payload, err := steerPayload(verb, keys, a.Fields)
	if err != nil {
		return err
	}

	key := engine.Key{AgentID: a.ID, Tab: panespec.TabAgent}
	in, ok := d.engine.Lookup(key)
	if ok {
		// The agent is up. An interrupt key that is destructive at an idle prompt
		// (Claude Code's Ctrl+C exits on a second press) is only ever sent mid-turn
		// — which is also the honest reading of the verb: `stop` interrupts the
		// current turn, and there isn't one.
		if verb == core.SteerStop && keys.InterruptOnlyWhileBusy &&
			h.Activity(sess) != engine.ActivityBusy {
			return fmt.Errorf("agent %s has no turn running to stop", a.ID)
		}
		if verb == core.SteerPermission {
			if err := checkPermissionRequest(h, sess, a.Fields[core.SteerRequestID]); err != nil {
				return err
			}
		}
		in.InputSequence(payload)
		return nil
	}

	// Only `prompt` starts a stopped agent — it reads as "talk to this session",
	// and having to start it first would be a distinction the caller can't see
	// from a remote rail. Interject/stop/permission all act on a turn that must
	// already be in flight, so starting one for them would be a surprise, not a
	// convenience.
	if verb != core.SteerPrompt {
		return fmt.Errorf("agent %s is not running (only %q starts a stopped agent)", a.ID, core.SteerPrompt)
	}
	// Everything this verb can be refused for has now been checked, so it is
	// accepted — and the cold start it needs takes seconds (a Claude Code boot),
	// which is far longer than the caller will wait. Hand the start to a goroutine
	// and return: the relay that carried this verb answers immediately, and the
	// progress arrives on the session's runtime-events stream instead.
	go d.startForSteer(ctx, a.ID, key, payload)
	return nil
}

// steerStructured serves a steering verb for a session under the App Server
// supervisor (AGE-181). Delivery is JSON-RPC, not keystrokes, so this is a
// complete alternative to the PTY payload path — same verbs, same "accepted"
// semantics. `prompt` runs a whole turn, so it is dispatched asynchronously (its
// progress arrives on the runtime-events stream) exactly like the PTY cold-start;
// the shorter verbs answer synchronously with the supervisor's own error. The
// permission verb correlates through the supervisor's approval tracker, which
// rejects a stale or duplicate request id (§3.1), so no separate open-prompt check
// is needed.
// structuredSteerer is the supervisor surface steering needs — the four verbs,
// nothing more. *codexapp.Supervisor satisfies it; a test injects a mock, so the
// routing/dispatch is covered without a real App Server.
type structuredSteerer interface {
	Prompt(ctx context.Context, text string) error
	Interject(ctx context.Context, text string) error
	Cancel(ctx context.Context) error
	Resolve(ctx context.Context, requestID, decision string) error
}

func (d *Daemon) steerStructured(ctx context.Context, id string, sup structuredSteerer, verb string, fields map[string]string) error {
	switch verb {
	case core.SteerPrompt:
		text := fields[core.SteerText]
		if text == "" {
			return fmt.Errorf("%s: need %q", verb, core.SteerText)
		}
		go d.runStructuredPrompt(ctx, id, sup, text)
		return nil
	case core.SteerInterject:
		text := fields[core.SteerText]
		if text == "" {
			return fmt.Errorf("%s: need %q", verb, core.SteerText)
		}
		return sup.Interject(ctx, text)
	case core.SteerStop:
		return sup.Cancel(ctx)
	case core.SteerPermission:
		switch fields[core.SteerDecision] {
		case core.SteerAllow, core.SteerDeny:
			return sup.Resolve(ctx, fields[core.SteerRequestID], fields[core.SteerDecision])
		default:
			return fmt.Errorf("permission: %q must be %q or %q, got %q",
				core.SteerDecision, core.SteerAllow, core.SteerDeny, fields[core.SteerDecision])
		}
	default:
		return fmt.Errorf("unknown steer verb %q", verb)
	}
}

// runStructuredPrompt drives one turn on a live supervisor after the verb was
// acknowledged. Like startForSteer it runs under the daemon's lifetime (not the
// caller's connection) and reports any failure to the session journal — the
// caller has already been told "accepted".
func (d *Daemon) runStructuredPrompt(ctx context.Context, id string, sup structuredSteerer, text string) {
	if d.steerStarted != nil {
		defer func() {
			select {
			case d.steerStarted <- id:
			default:
			}
		}()
	}
	if err := sup.Prompt(ctx, text); err != nil {
		structuredJournal(id, core.JournalError, fmt.Sprintf("prompt: %v", err))
	}
}

// startStructuredForPrompt starts a stopped structured session's App Server and
// delivers the first prompt, all after the verb is acknowledged (a cold start
// takes seconds). Progress and any failure reach the caller as journal/notice
// events on the session's runtime-events stream, mirroring startForSteer.
func (d *Daemon) startStructuredForPrompt(ctx context.Context, sess store.Session, fields map[string]string) error {
	text := fields[core.SteerText]
	if text == "" {
		return fmt.Errorf("%s: need %q", core.SteerPrompt, core.SteerText)
	}
	go func() {
		structuredJournal(sess.ID, core.JournalInfo, "starting agent")
		sup, err := d.ensureSupervisor(sess.ID)
		if err != nil {
			// steerStartFailed writes the human journal + daemon log; also surface the
			// failure on the session's single structured event source so a subscriber
			// waiting on the first turn sees why it never came.
			structuredNotice(sess.ID, core.JournalError, fmt.Sprintf("start agent %s: %v", sess.ID, err))
			d.steerStartFailed(sess.ID, fmt.Errorf("start agent %s: %w", sess.ID, err))
			return
		}
		d.triggerPoll()
		d.runStructuredPrompt(ctx, sess.ID, sup, text)
	}()
	return nil
}

// startForSteer performs the work an accepted `prompt` to a stopped agent
// deferred: bring the engine instance up, then type the text into it. It runs
// after the caller has been answered, so nothing it does can be reported by
// returning an error — every outcome is written to the session's journal
// instead, which reaches a subscribed orchestrator as a `notice` event on the
// same stream the agent's own output will arrive on (docs/
// remote-provider-sessions.md §4.6).
//
// ctx is the daemon's lifetime context, not the connection's: the client that
// sent the verb is gone the moment it reads the ack, and a start cancelled by
// its disconnect would leave the session exactly as stuck as the timeout this
// change exists to remove.
func (d *Daemon) startForSteer(ctx context.Context, id string, key engine.Key, payload []engine.InputStep) {
	if d.steerStarted != nil {
		defer func() {
			select {
			case d.steerStarted <- id:
			default:
			}
		}()
	}
	journal(id, core.JournalInfo, "starting agent")
	// Start exactly the steered session: a prompt to a workgroup id is a prompt to
	// its coordinator, not a "start every member" (which is what `start <root>`
	// means — see startEngineFor).
	if err := d.startAgent(ctx, id); err != nil {
		d.steerStartFailed(id, fmt.Errorf("start agent %s: %w", id, err))
		return
	}
	d.triggerPoll()
	in, ok := d.engine.Lookup(key)
	if !ok {
		d.steerStartFailed(id, fmt.Errorf("agent %s did not come up", id))
		return
	}
	// The runtime is a TUI that has to boot before it will accept typed input, so
	// reserve a delay in the input FIFO instead of sleeping on this goroutine. The
	// verb is already "accepted" at this point; this is not a readiness check.
	d.deferInput(in, payload)
}

// steerStartFailed reports a start that failed after the verb was already
// acknowledged. It goes to the session's journal — where the orchestrator that
// sent the verb is watching — and to the daemon log, where a local operator is.
// Neither is optional: the caller has been told "accepted" and would otherwise
// wait forever for a turn that is never coming.
func (d *Daemon) steerStartFailed(id string, err error) {
	log.Printf("steer: %v", err)
	journal(id, core.JournalError, err.Error())
}

// journal appends one line to a session's amux journal, logging a write that
// fails rather than propagating it: the journal is a progress report, and losing
// a line must never be worse than the condition it was reporting.
func journal(id, level, text string) {
	if err := core.AppendJournal(id, level, text); err != nil {
		log.Printf("steer: journal %s: %v", id, err)
	}
}

// structuredJournal reports a structured session's cold-start/failure progress to
// BOTH the human session journal and the session's single structured runtime-event
// log — so the notice reaches a subscriber tailing that one canonical source
// (runtimeRecord resolves a structured session to it alone) as well as the CLI
// journal. Use only for structured (App Server) sessions; a stray call would create
// an events file for a session that has none.
func structuredJournal(id, level, text string) {
	journal(id, level, text)
	structuredNotice(id, level, text)
}

// structuredNotice appends a notice to the session's structured event log only,
// for a site that already recorded the human journal + daemon log elsewhere.
// Best-effort, like journal.
func structuredNotice(id, level, text string) {
	if err := codexapp.AppendNotice(id, level, text); err != nil {
		log.Printf("steer: structured notice %s: %v", id, err)
	}
}

// steerTarget resolves a session id to the harness that knows how to drive it and
// the record naming its conversation — the two things every steering decision
// needs, read once so the store isn't opened twice per verb.
func (d *Daemon) steerTarget(agentID string) (agent.Harness, store.Session, error) {
	s, ok, err := lookupSession(agentID)
	if err != nil {
		return nil, store.Session{}, fmt.Errorf("read session %s: %w", agentID, err)
	}
	if !ok {
		return nil, store.Session{}, fmt.Errorf("no such session %s", agentID)
	}
	return agent.HarnessFor(s.Agent), s, nil
}

// checkPermissionRequest refuses a `permission` verb whose request_id does not
// name a prompt the runtime currently has open. Without it the decision races:
// if the turn moves on between the orchestrator seeing a prompt and its verb
// arriving, the allow/deny keystroke lands on a *different* prompt and is applied
// to an action nobody approved. Refusing is the only safe answer — the caller can
// re-read the runtime-events stream and decide again.
//
// The open set is replayed through the same readers that publish the stream
// (runtimeevents), so the daemon can only ever accept an id a consumer was
// actually told about.
//
// An empty request_id is still accepted, as it was when the field shipped
// advisory (AGE-160): a caller that quotes nothing is explicitly asking for
// "whatever prompt is open", and this verb is its only way to say so.
func checkPermissionRequest(h agent.Harness, s store.Session, requestID string) error {
	if requestID == "" {
		return nil
	}
	open := runtimeevents.OpenPermissions(permissionRecord(h, s))
	ids := make([]string, 0, len(open))
	for _, p := range open {
		if p.RequestID == requestID {
			return nil
		}
		ids = append(ids, p.RequestID)
	}
	if len(ids) == 0 {
		return fmt.Errorf("permission: no pending request %q (the runtime has no prompt open)", requestID)
	}
	return fmt.Errorf("permission: no pending request %q (the runtime is waiting on %s)",
		requestID, strings.Join(ids, ", "))
}

// permissionRecord names the on-disk records a session's permission prompts can
// be read from: the runtime transcript, plus amux's own journal for a runtime
// that resolves its prompts without recording them. It mirrors what the provider
// hands the runtime-events reader (daemon.runtimeRecord), so both see one story.
func permissionRecord(h agent.Harness, s store.Session) runtimeevents.Record {
	path, ok := h.RuntimeTranscriptPath(s)
	if !ok {
		return runtimeevents.Record{}
	}
	perms, _ := h.RuntimePermissionPath(s)
	return runtimeevents.Record{
		Runtime: agent.Canonical(s.Agent), Path: path, Permissions: perms,
	}
}

// lookupSession resolves a session id the way the rail and the pane resolver do
// (wsops.ResolveSession): the built-in console is a synthetic session (never a
// store row); a workgroup id resolves to its coordinator and a repo name to its
// home session — the built-in default sessions — and everything else is read
// from the store. Every daemon-side id → session lookup goes through here so a
// verb that reaches the daemon by id — steering from a remote rail included —
// sees the same inventory the daemon publishes.
func lookupSession(id string) (store.Session, bool, error) {
	return wsops.ResolveSession(id)
}

// steerPayload turns a verb plus its fields into the byte writes that serve it,
// in order. The harness supplies paste framing and a delay before submission;
// separate immediate writes alone can coalesce into one TUI paste event.
func steerPayload(verb string, keys agent.Keys, fields map[string]string) ([]engine.InputStep, error) {
	switch verb {
	case core.SteerPrompt, core.SteerInterject:
		text := fields[core.SteerText]
		if text == "" {
			return nil, fmt.Errorf("%s: need %q", verb, core.SteerText)
		}
		// An embedded paste delimiter would escape the text frame and become
		// keystrokes. Refuse it rather than silently changing the requested text.
		if len(keys.PasteEnd) > 0 && (strings.Contains(text, string(keys.PasteStart)) || strings.Contains(text, string(keys.PasteEnd))) {
			return nil, fmt.Errorf("%s: text contains a terminal paste delimiter", verb)
		}
		pasted := append(append(append([]byte(nil), keys.PasteStart...), []byte(text)...), keys.PasteEnd...)
		return []engine.InputStep{{Bytes: pasted}, {Bytes: keys.Submit, DelayBefore: keys.SubmitDelay}}, nil
	case core.SteerStop:
		return []engine.InputStep{{Bytes: keys.Interrupt}}, nil
	case core.SteerPermission:
		switch fields[core.SteerDecision] {
		case core.SteerAllow:
			return []engine.InputStep{{Bytes: keys.Allow}}, nil
		case core.SteerDeny:
			return []engine.InputStep{{Bytes: keys.Deny}}, nil
		default:
			// Never guess at a permission prompt: an unparseable decision is refused
			// rather than resolved in either direction.
			return nil, fmt.Errorf("permission: %q must be %q or %q, got %q",
				core.SteerDecision, core.SteerAllow, core.SteerDeny, fields[core.SteerDecision])
		}
	default:
		return nil, fmt.Errorf("unknown steer verb %q", verb)
	}
}

// deferInput reserves the startup wait in the same FIFO as subsequent input,
// so a second prompt cannot overtake the prompt that started the runtime.
func (d *Daemon) deferInput(in engine.Instance, payload []engine.InputStep) {
	settle := d.steerSettle
	if settle == 0 {
		settle = steerStartSettle
	}
	payload[0].DelayBefore += settle
	in.InputSequence(payload)
}
