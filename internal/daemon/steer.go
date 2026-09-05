package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/runtimeevents"
	"amux/internal/store"
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
	if err := d.startEngineFor(ctx, a.ID); err != nil {
		return fmt.Errorf("start agent %s: %w", a.ID, err)
	}
	d.triggerPoll()
	in, ok = d.engine.Lookup(key)
	if !ok {
		return fmt.Errorf("agent %s did not come up", a.ID)
	}
	// The runtime is a TUI that has to boot before it will accept typed input, so
	// reserve a delay in the input FIFO instead of sleeping on the caller. The
	// verb is already "accepted" at this point; this is not a readiness check.
	d.deferInput(in, payload)
	return nil
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

// lookupSession resolves a session id the way the rail and the pane resolver do:
// the built-in console is a synthetic session (never a store row), so it answers
// from the console package; everything else is read from the store. Every
// daemon-side id → session lookup goes through here so a verb that reaches the
// daemon by id — steering from a remote rail included — sees the same inventory
// the daemon publishes.
func lookupSession(id string) (store.Session, bool, error) {
	if id == console.ID {
		return console.Session(), true, nil
	}
	db, err := store.Open()
	if err != nil {
		return store.Session{}, false, fmt.Errorf("open store: %w", err)
	}
	defer db.Close()
	return db.GetSession(id)
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
