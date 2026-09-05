package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/store"
)

// isolateHome points the store (and every config path) at a temp dir so a test
// never reads or writes the developer's real amux data.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "off")
}

// putSession stores a minimal session of the given agent kind, which is all
// steerKeys reads to pick a keystroke mapping.
func putSession(t *testing.T, id, kind string) {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.PutSession(store.Session{
		ID: id, Name: id, Agent: kind, Dir: t.TempDir(), ClaudeID: convID(id),
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
}

// convID is the pinned conversation id a test session carries, which is the key
// the harness's turn-state lookup uses.
func convID(agentID string) string { return "conv-" + agentID }

// markBusy writes the hook record Claude's harness reads for its turn state, so a
// test can put a session mid-turn — the condition `stop` requires before it will
// send Claude Code's Ctrl+C.
func markBusy(t *testing.T, convID string) {
	t.Helper()
	if err := core.WriteHookState(convID, core.StateRunning, ""); err != nil {
		t.Fatalf("write hook state: %v", err)
	}
}

// fakeInstance records what was written to it instead of owning a PTY.
type fakeInstance struct {
	key   engine.Key
	mu    sync.Mutex
	wrote [][]byte
	dead  bool
}

func (f *fakeInstance) Key() engine.Key              { return f.key }
func (f *fakeInstance) Subscribe(engine.Sink) func() { return func() {} }
func (f *fakeInstance) Resize(int, int)              {}
func (f *fakeInstance) Alive() bool                  { return !f.dead }
func (f *fakeInstance) Input(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrote = append(f.wrote, append([]byte(nil), p...))
}

// written joins the recorded writes, which is what the runtime's PTY would see.
func (f *fakeInstance) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, w := range f.wrote {
		b.Write(w)
	}
	return b.String()
}

// fakeEngine holds instances by key; Ensure registers one, mimicking a start.
type fakeEngine struct {
	mu        sync.Mutex
	insts     map[engine.Key]*fakeInstance
	ensureErr error
	ensured   []engine.Key
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{insts: map[engine.Key]*fakeInstance{}}
}

func (e *fakeEngine) Name() string { return "fake" }
func (e *fakeEngine) Ensure(_ context.Context, spec engine.Spec) (engine.Instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensured = append(e.ensured, spec.Key)
	if e.ensureErr != nil {
		return nil, e.ensureErr
	}
	in, ok := e.insts[spec.Key]
	if !ok {
		in = &fakeInstance{key: spec.Key}
		e.insts[spec.Key] = in
	}
	return in, nil
}
func (e *fakeEngine) Lookup(key engine.Key) (engine.Instance, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in, ok := e.insts[key]
	if !ok {
		return nil, false
	}
	return in, true
}
func (e *fakeEngine) Live() []engine.Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]engine.Key, 0, len(e.insts))
	for k := range e.insts {
		out = append(out, k)
	}
	return out
}
func (e *fakeEngine) Kill(key engine.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.insts, key)
}
func (e *fakeEngine) Shutdown() {}

// running registers an already-live agent pane for id.
func (e *fakeEngine) running(id string) *fakeInstance {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := &fakeInstance{key: engine.Key{AgentID: id, Tab: panespec.TabAgent}}
	e.insts[in.key] = in
	return in
}

// steerDaemon is a daemon with a fake engine and an isolated store.
func steerDaemon(t *testing.T) (*Daemon, *fakeEngine) {
	t.Helper()
	isolateHome(t)
	eng := newFakeEngine()
	d := New("", nil, time.Hour)
	d.engine = eng
	d.steerSettle = time.Millisecond
	d.agentsUnder = func(id string) ([]string, error) { return []string{id}, nil }
	d.resolve = func(string, int) (string, []string, []string, error) { return "", nil, []string{"sh"}, nil }
	return d, eng
}

// TestSteerDeliversKeystrokes is the heart of the feature: each verb must reach
// the agent pane as the exact bytes that runtime's TUI expects. The expected
// strings are spelled out literally here rather than read from agent.Keys, so a
// change to a mapping has to be made deliberately in two places instead of a
// test silently agreeing with a regression.
func TestSteerDeliversKeystrokes(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		fields map[string]string
		want   string
	}{
		{"claude prompt", "claude",
			map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "run the tests"},
			"run the tests\r"},
		{"claude interject", "claude",
			map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: "skip the flaky one"},
			"skip the flaky one\r"},
		{"claude stop", "claude",
			map[string]string{core.SteerVerb: core.SteerStop}, "\x03"},
		{"claude allow", "claude",
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerAllow},
			"\r"},
		{"claude deny", "claude",
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerDeny},
			"\x1b"},
		{"codex prompt", "codex",
			map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "hi"}, "hi\r"},
		{"codex stop", "codex",
			map[string]string{core.SteerVerb: core.SteerStop}, "\x1b"},
		{"codex allow", "codex",
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerAllow}, "y"},
		{"codex deny", "codex",
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerDeny}, "n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, eng := steerDaemon(t)
			putSession(t, "a1", tc.kind)
			in := eng.running("a1")
			// `stop` only fires mid-turn for a harness whose interrupt key is unsafe
			// at an idle prompt, so put the session in a turn.
			if tc.fields[core.SteerVerb] == core.SteerStop {
				markBusy(t, convID("a1"))
			}

			if err := d.steer(context.Background(), core.Action{
				Action: core.ActionSteer, ID: "a1", Fields: tc.fields,
			}); err != nil {
				t.Fatalf("steer: %v", err)
			}
			if got := in.written(); got != tc.want {
				t.Fatalf("pane received %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSteerRefusals proves every refusal is explicit and typed rather than a
// silent no-op — a steering verb that can't be delivered must say so, because
// the caller is a remote orchestrator with no view of this machine.
func TestSteerRefusals(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		running bool
		fields  map[string]string
		id      string
		wantSub string
	}{
		{"interject on a stopped agent", "claude", false,
			map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: "x"}, "a1", "is not running"},
		{"stop on a stopped agent", "claude", false,
			map[string]string{core.SteerVerb: core.SteerStop}, "a1", "is not running"},
		{"permission on a stopped agent", "claude", false,
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerAllow}, "a1", "is not running"},
		{"prompt with no text", "claude", true,
			map[string]string{core.SteerVerb: core.SteerPrompt}, "a1", `need "text"`},
		{"unparseable decision", "claude", true,
			map[string]string{core.SteerVerb: core.SteerPermission, core.SteerDecision: "maybe"}, "a1", "must be"},
		{"missing decision", "claude", true,
			map[string]string{core.SteerVerb: core.SteerPermission}, "a1", "must be"},
		{"unknown verb", "claude", true,
			map[string]string{core.SteerVerb: "detonate"}, "a1", "unknown steer verb"},
		{"unsteerable kind", "hermes", true,
			map[string]string{core.SteerVerb: core.SteerStop}, "a1", "no steering keys"},
		{"no such session", "claude", true,
			map[string]string{core.SteerVerb: core.SteerStop}, "nope", "no such session"},
		{"no id", "claude", true,
			map[string]string{core.SteerVerb: core.SteerStop}, "", "need a session id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, eng := steerDaemon(t)
			putSession(t, "a1", tc.kind)
			var in *fakeInstance
			if tc.running {
				in = eng.running("a1")
			}
			err := d.steer(context.Background(), core.Action{
				Action: core.ActionSteer, ID: tc.id, Fields: tc.fields,
			})
			if err == nil {
				t.Fatalf("steer succeeded, want an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.wantSub)
			}
			if in != nil && in.written() != "" {
				t.Fatalf("a refused verb still wrote %q to the pane", in.written())
			}
		})
	}
}

// TestStopRefusesAnIdleClaude guards the sharpest edge in the feature. Claude
// Code's interrupt is Ctrl+C, and at an *idle* prompt a second Ctrl+C exits the
// CLI — so a caller repeating `stop` at a session with no turn running would kill
// the session the verb promises to keep alive. A live-but-idle agent must refuse.
func TestStopRefusesAnIdleClaude(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	in := eng.running("a1") // running, but no turn in flight

	err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerStop},
	})
	if err == nil || !strings.Contains(err.Error(), "no turn running") {
		t.Fatalf("error = %v, want a refusal because no turn is running", err)
	}
	if got := in.written(); got != "" {
		t.Fatalf("sent %q to an idle Claude — a second one of those exits the CLI", got)
	}

	// Mid-turn the same verb goes through.
	markBusy(t, convID("a1"))
	if err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerStop},
	}); err != nil {
		t.Fatalf("steer mid-turn: %v", err)
	}
	if got := in.written(); got != "\x03" {
		t.Fatalf("pane received %q, want Ctrl+C", got)
	}
}

// TestStopNeedsNoTurnForAnInertKey is the contrast: Codex's interrupt is Esc,
// which is inert at an idle composer rather than destructive, so its `stop` is
// not gated on a turn state Codex doesn't report anyway. The guard has to be a
// property of the key, not a blanket rule, or steering Codex would never work.
func TestStopNeedsNoTurnForAnInertKey(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "codex")
	in := eng.running("a1")

	if err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerStop},
	}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	if got := in.written(); got != "\x1b" {
		t.Fatalf("pane received %q, want Esc", got)
	}
}

// TestSteerPromptStartsStoppedAgent pins the one verb that starts an agent: a
// prompt to a stopped session brings it up and then types the text, so a remote
// caller doesn't have to know whether the process happens to be running.
func TestSteerPromptStartsStoppedAgent(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")

	if err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1",
		Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
	}); err != nil {
		t.Fatalf("steer: %v", err)
	}

	key := engine.Key{AgentID: "a1", Tab: panespec.TabAgent}
	if len(eng.ensured) != 1 || eng.ensured[0] != key {
		t.Fatalf("ensured = %v, want exactly the agent pane %v", eng.ensured, key)
	}
	inst, ok := eng.Lookup(key)
	if !ok {
		t.Fatal("no instance after a prompt to a stopped agent")
	}
	in := inst.(*fakeInstance)
	// The write is deferred until the TUI has had time to paint, so poll for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && in.written() != "go\r" {
		time.Sleep(2 * time.Millisecond)
	}
	if got := in.written(); got != "go\r" {
		t.Fatalf("pane received %q, want %q", got, "go\r")
	}
}

// TestSteerReachesTheConsole pins the one session that is not a store row. The
// console is published in the same inventory as every agent, so a remote rail
// that prompts it must be answered like any other session — a store-only lookup
// answered "no such session console" to exactly the row the daemon had just
// advertised. Both halves matter: a running console takes the keystrokes
// directly, and a stopped one is started by `prompt` like any other agent.
func TestSteerReachesTheConsole(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		d, eng := steerDaemon(t)
		in := eng.running(console.ID)
		if err := d.steer(context.Background(), core.Action{
			Action: core.ActionSteer, ID: console.ID,
			Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "add a repo"},
		}); err != nil {
			t.Fatalf("steer: %v", err)
		}
		if got := in.written(); got != "add a repo\r" {
			t.Fatalf("pane received %q, want %q", got, "add a repo\r")
		}
	})
	t.Run("stopped", func(t *testing.T) {
		d, eng := steerDaemon(t)
		if err := d.steer(context.Background(), core.Action{
			Action: core.ActionSteer, ID: console.ID,
			Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
		}); err != nil {
			t.Fatalf("steer: %v", err)
		}
		key := engine.Key{AgentID: console.ID, Tab: panespec.TabAgent}
		inst, ok := eng.Lookup(key)
		if !ok {
			t.Fatal("no console instance after a prompt to the stopped console")
		}
		in := inst.(*fakeInstance)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && in.written() != "go\r" {
			time.Sleep(2 * time.Millisecond)
		}
		if got := in.written(); got != "go\r" {
			t.Fatalf("pane received %q, want %q", got, "go\r")
		}
	})
}

// TestSteerPromptReportsStartFailure keeps the start path honest: if the agent
// can't come up, the verb fails loudly instead of reporting a delivery that
// never happened.
func TestSteerPromptReportsStartFailure(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	eng.ensureErr = os.ErrPermission

	err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1",
		Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
	})
	if err == nil || !strings.Contains(err.Error(), "start agent a1") {
		t.Fatalf("error = %v, want a start failure for a1", err)
	}
}

// TestSteerDoesNotSurviveADeadPane guards the deferred write: an agent that
// exits between the start and the settle must not be typed into.
func TestSteerDoesNotSurviveADeadPane(t *testing.T) {
	d, _ := steerDaemon(t)
	in := &fakeInstance{dead: true}
	d.deferInput(in, [][]byte{[]byte("go"), []byte("\r")})
	time.Sleep(20 * time.Millisecond)
	if got := in.written(); got != "" {
		t.Fatalf("wrote %q to a dead pane", got)
	}
}

// TestKeystrokeMappingLivesInTheRegistry is the contract the ticket asks for:
// which bytes a runtime expects is agent-kind knowledge, so it belongs to the
// agent-kind registry (internal/agent) and nowhere else. If the daemon ever
// grows its own `if kind == "claude"` or a bare escape byte, this fails — the
// exact drift HarnessFor exists to prevent.
func TestKeystrokeMappingLivesInTheRegistry(t *testing.T) {
	src, err := os.ReadFile("steer.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`\x1b`, `\r"`, `"claude"`, `"codex"`, `"hermes"`} {
		if strings.Contains(string(src), banned) {
			t.Errorf("steer.go contains %s — keystrokes and agent kinds belong in internal/agent, "+
				"behind Harness.Keys, not in the daemon", banned)
		}
	}
}
