package agent

import "github.com/kchymet/agent-multiplexer/harnessproto"

// caps.go is amux's per-session capability producer (AGE-178): it turns an agent
// kind into the honest control surface the daemon can actually serve for a
// published session (docs/remote-provider-sessions.md §3.1), so a remote
// orchestrator gates its affordances on what will work rather than on whether a
// transcript happens to exist.
//
// The caps are derived from the same two facts the daemon's steer handler acts
// on, so the advertisement can never claim more than steering can deliver:
//
//   - the harness's steering Keys (agent/keys.go) — which keystrokes drive this
//     runtime's TUI. prompt/interject ride Submit, cancel rides Interrupt, and a
//     permission decision needs both Allow and Deny (never guess a prompt).
//   - whether the runtime raises CORRELATED permission_request events — a
//     request_id the VerbPermission verb can quote back and the daemon can match
//     to an open prompt (AGE-172). Transcript support alone is not enough: a
//     runtime that streams a transcript but records no answerable prompt reports
//     Permission=false.

// CapsFor returns the control capabilities amux advertises for a published
// session of the given agent kind. It is a static per-kind property — the caps
// describe what the runtime's TUI can be driven to do, not the momentary state of
// one session — so the producer may call it freely while building the inventory.
//
// An unrecognized kind resolves to a no-op harness with empty Keys, so its caps
// come back all-false: the honest answer for a runtime amux cannot steer, which a
// consumer renders as disabled controls rather than affordances that only error.
func CapsFor(kind string) harnessproto.SessionCaps {
	h := HarnessFor(kind)
	k := h.Keys()
	return harnessproto.SessionCaps{
		Prompt:     len(k.Submit) > 0,
		Interject:  len(k.Submit) > 0,
		Cancel:     len(k.Interrupt) > 0,
		Permission: len(k.Allow) > 0 && len(k.Deny) > 0 && correlatesPermissions(h.Kind()),
	}
}

// correlatesPermissions reports whether a runtime raises permission_request
// events with a request_id a VerbPermission verb can answer and the daemon can
// correlate to an open prompt. These are exactly the runtimes amux has a
// runtime-events permission source for (internal/runtimeevents.sourcesFor):
// Claude via amux's own permission journal, Codex via its rollout. A runtime
// without such a source cannot back an answerable approval round-trip, so it
// reports Permission=false even when its TUI has allow/deny keys.
//
// It is spelled here rather than reached out of the runtimeevents package to keep
// the agent registry free of that dependency. This set MUST stay in step with the
// permission sources in internal/runtimeevents/tailer.go (sourcesFor): a runtime
// added there without being added here would advertise Permission=false despite
// producing correlatable prompts, and vice versa would advertise a phantom.
func correlatesPermissions(kind string) bool {
	switch kind {
	case harnessproto.RuntimeClaude, harnessproto.RuntimeCodex:
		return true
	default:
		return false
	}
}
