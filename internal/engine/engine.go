// Package engine abstracts how and where an agent runs. An Engine instantiates
// agent "instances" in some environment and owns their lifetime independently of
// any UI: the daemon holds one engine, asks it to ensure/stream/kill instances,
// and clients (the native TUI, a remote UI) merely attach to them. Because the
// daemon — not the UI — owns the engine, closing or restarting a UI never stops
// an agent.
//
// The local engine (internal/engine/local) runs each instance as a PTY-backed
// process on this machine, much like amux always has. The interface deliberately
// keeps amux's session model out of it (the daemon resolves a Spec and hands it
// over), so other engines can run agents elsewhere — e.g. a future cloud engine
// that instantiates an agent on Kubernetes or another infra provider with a
// non-terminal backend. Such an engine implements the same interface; only the
// transport behind Subscribe/Input differs.
package engine

import "context"

// Key identifies one running instance. An agent exposes several panes (the agent
// process, an editor, a shell), so the key is the agent id plus its tab. It is
// the stable handle a reconnecting client re-attaches by.
type Key struct {
	AgentID string
	Tab     int
}

// Activity is an instance's turn state as far as a graceful stop cares: whether
// stopping it now is safe or would interrupt work in flight. It is deliberately
// abstract — an engine consults it (via ActivityFunc) without knowing how the
// signal is produced. For a harness like Claude Code it maps from the agent's
// reported hook state (see internal/agent.Harness), but any harness can supply
// it however it likes.
type Activity int

const (
	// ActivityUnknown means there is no signal for the instance; callers treat it
	// as safe to stop (it never blocks a shutdown on a missing signal).
	ActivityUnknown Activity = iota
	// ActivitySafe means the instance is idle/between turns; stopping it now loses
	// nothing.
	ActivitySafe
	// ActivityBusy means the instance is mid-turn; stopping it now may interrupt
	// work the harness hasn't yet persisted, so a graceful stop should wait.
	ActivityBusy
)

// ActivityFunc reports the current Activity of an instance by key. An engine
// consults it on a graceful stop to defer terminating busy instances until they
// are safe. A nil ActivityFunc (or one returning ActivityUnknown) means "always
// safe to stop", preserving the stop-immediately behavior.
type ActivityFunc func(Key) Activity

// Spec is a fully-resolved request to run one instance: what to launch, where,
// and the initial terminal size. The daemon resolves it (working dir, env, argv,
// sandboxing) before handing it to the engine, so the engine needs no knowledge
// of repos, worktrees, or Claude resume logic.
type Spec struct {
	Key  Key
	Dir  string   // working directory
	Env  []string // KEY=VALUE additions to the engine's base environment
	Argv []string // command + args
	Cols int      // initial terminal width
	Rows int      // initial terminal height
}

// Sink receives an instance's output and its eventual exit. Both callbacks fire
// from the engine's own goroutines; an implementation must not block in them.
// For a terminal-backed engine Output carries raw PTY bytes (escape sequences
// and all); a non-terminal engine is free to define its own framing on the same
// channel, with the UI selecting a matching renderer by engine.
type Sink struct {
	Output func([]byte)
	Exit   func(errMsg string)
}

// Instance is one running agent pane. Several subscribers may attach at once;
// each gets a replay of recent output followed by the live stream, so a UI that
// reconnects repaints immediately. Detaching a subscriber never stops the
// instance — only Engine.Kill (or the process exiting) does.
type Instance interface {
	Key() Key
	// Subscribe attaches a sink: it first replays buffered scrollback through
	// Output, then streams live output, until the returned cancel is called. If
	// the instance has already exited, Subscribe still replays the buffer and then
	// invokes Exit. cancel is idempotent.
	Subscribe(Sink) (cancel func())
	// Input forwards bytes (e.g. keystrokes) to the instance.
	Input([]byte)
	// Resize sets the instance's terminal size.
	Resize(cols, rows int)
	// Alive reports whether the underlying process/workload is still running.
	Alive() bool
}

// Engine instantiates and runs agent instances in one environment.
type Engine interface {
	// Name identifies the engine ("local", "k8s", …).
	Name() string
	// Ensure starts the instance for spec if it isn't already running and returns
	// it; if one already runs for spec.Key it is returned unchanged (idempotent).
	Ensure(ctx context.Context, spec Spec) (Instance, error)
	// Lookup returns the running instance for key, or false.
	Lookup(key Key) (Instance, bool)
	// Live returns the keys of every instance currently running.
	Live() []Key
	// Kill terminates the instance for key (no-op if absent).
	Kill(key Key)
	// Shutdown terminates every instance the engine owns.
	Shutdown()
}
