package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/runtimeevents"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
	"github.com/kchymet/agent-multiplexer/harnessproto"
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
		ID: id, RootID: "test-root", Name: id, Agent: kind, Dir: t.TempDir(), ClaudeID: convID(id),
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
func markBusy(t *testing.T, subjectID string) {
	t.Helper()
	if err := core.WriteSessionHookState(subjectID, convID(subjectID), core.StateRunning, ""); err != nil {
		t.Fatalf("write hook state: %v", err)
	}
}

// fakeInstance records what was written to it instead of owning a PTY.
type fakeInstance struct {
	key       engine.Key
	mu        sync.Mutex
	wrote     [][]byte
	sequences [][]engine.InputStep
	dead      bool
}

func (f *fakeInstance) Key() engine.Key              { return f.key }
func (f *fakeInstance) Subscribe(engine.Sink) func() { return func() {} }
func (f *fakeInstance) Resize(int, int)              {}
func (f *fakeInstance) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead
}
func (f *fakeInstance) Input(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wrote = append(f.wrote, append([]byte(nil), p...))
}

func (f *fakeInstance) InputSequence(steps []engine.InputStep) {
	f.mu.Lock()
	f.sequences = append(f.sequences, steps)
	f.mu.Unlock()
	if f.Alive() {
		for _, step := range steps {
			f.Input(step.Bytes)
		}
	}
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
// ensureBlock, when set, parks Ensure until it is closed — the stand-in for a
// runtime cold start, which is what an accepted `prompt` must not wait on.
type fakeEngine struct {
	mu          sync.Mutex
	insts       map[engine.Key]*fakeInstance
	ensureErr   error
	ensureBlock chan struct{}
	// ensurePublished runs after the replacement is visible through Lookup but
	// before Ensure returns, reproducing the production publication interval.
	ensurePublished func(engine.Instance)
	killObserved    func(engine.Instance)
	killRefuses     bool
	ensured         []engine.Key
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{insts: map[engine.Key]*fakeInstance{}}
}

func TestStartAgentRevalidatesAtRuntimeExecution(t *testing.T) {
	d := New("", nil, time.Hour)
	eng := newFakeEngine()
	d.engine = eng
	allowed := true
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{Session: store.Session{ID: "a1", Agent: "claude", Dir: t.TempDir()}}, nil
	}
	d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		allowed = false // policy changes after resolution but before Engine.Ensure
		return t.TempDir(), nil, []string{"agent"}, nil
	}
	ctx := withAccessGuard(context.Background(), func() error {
		if !allowed {
			return access.ErrDenied
		}
		return nil
	})
	if err := d.startAgent(ctx, "a1"); err == nil {
		t.Fatal("runtime started after its execution policy was revoked")
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.ensured) != 0 {
		t.Fatalf("engine Ensure called after revocation: %v", eng.ensured)
	}
}

type gatedPromptSteerer struct {
	started chan struct{}
	release chan struct{}
}

func (s *gatedPromptSteerer) Prompt(ctx context.Context, text string) error {
	wait, err := s.BeginPrompt(ctx, text)
	if err != nil {
		return err
	}
	return wait(ctx)
}
func (s *gatedPromptSteerer) BeginPrompt(context.Context, string) (func(context.Context) error, error) {
	close(s.started)
	return func(ctx context.Context) error {
		select {
		case <-s.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, nil
}
func (*gatedPromptSteerer) Interject(context.Context, string) error       { return nil }
func (*gatedPromptSteerer) Cancel(context.Context) error                  { return nil }
func (*gatedPromptSteerer) Resolve(context.Context, string, string) error { return nil }

func TestDeferredStructuredPromptSerializesAdmissionButNotModelTurn(t *testing.T) {
	root := store.Session{ID: "prompt-root", Scope: store.ScopeWork, Agent: "claude", Dir: t.TempDir()}
	other := store.Session{ID: "prompt-other", Scope: store.ScopeWork, Agent: "claude", Dir: t.TempDir()}
	member := store.Session{ID: "prompt-member", RootID: root.ID, Agent: "claude", Dir: t.TempDir()}
	d, runtime, principals := sessionRuntimeFixture(t, root, other, member)
	d.sessionRPC = runtime
	d.steerStarted = make(chan string, 1)
	defer runtime.close()
	call := sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionSteer, ID: member.ID,
		Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "bounded prompt"},
	}
	if err := runtime.authorize(context.Background(), principals[root.ID], call); err != nil {
		t.Fatal(err)
	}

	validationEntered := make(chan struct{})
	releaseValidation := make(chan struct{})
	guarded := withAccessGuard(withEffectAdmission(context.Background()), func() error {
		close(validationEntered)
		<-releaseValidation
		return runtime.authorize(context.Background(), principals[root.ID], call)
	})
	sink := &gatedPromptSteerer{started: make(chan struct{}), release: make(chan struct{})}
	if err := d.steerStructured(guarded, member.ID, sink, core.SteerPrompt, call.Fields); err != nil {
		t.Fatal(err)
	}
	<-validationEntered

	moved := make(chan core.Result, 1)
	go func() {
		moved <- d.handle(context.Background(), core.Action{Action: core.ActionMove, ID: member.ID, Target: other.ID})
	}()
	select {
	case result := <-moved:
		t.Fatalf("host move crossed final prompt admission: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case <-sink.started:
		t.Fatal("prompt started before its final policy validation completed")
	default:
	}

	close(releaseValidation)
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("prompt turn/start was not admitted")
	}
	select {
	case result := <-moved:
		if !result.OK {
			t.Fatalf("host move failed after prompt admission: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("host move remained blocked for the model turn")
	}
	if err := runtime.authorize(context.Background(), principals[root.ID], call); err == nil {
		t.Fatal("old coordinator remained authorized after member move")
	}
	close(sink.release)
	select {
	case <-d.steerStarted:
	case <-time.After(time.Second):
		t.Fatal("structured prompt waiter did not finish")
	}
}

func TestStartAgentPublishesReplacementGenerationAtomically(t *testing.T) {
	isolateHome(t)
	d := New("", nil, time.Hour)
	d.permissionBaseline = func(string) ([]string, error) { return nil, nil }
	eng := newFakeEngine()
	d.engine = eng
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{Session: store.Session{ID: "a1", Agent: "claude", Dir: t.TempDir()}}, nil
	}
	d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return t.TempDir(), nil, []string{"agent"}, nil
	}
	old := eng.running("a1")
	oldGeneration, err := d.permissions.observe("a1", old)
	if err != nil {
		t.Fatal(err)
	}
	bindPermission(t, d.permissions, "a1", "request-old", old)
	eng.mu.Lock()
	delete(eng.insts, old.Key())
	eng.mu.Unlock()
	published := make(chan struct{})
	releaseEnsure := make(chan struct{})
	eng.ensurePublished = func(engine.Instance) {
		close(published)
		<-releaseEnsure
	}
	started := make(chan error, 1)
	go func() { started <- d.startAgent(context.Background(), "a1") }()
	<-published

	delivered := make(chan error, 1)
	go func() {
		delivered <- d.permissions.consume("a1", oldGeneration, "request-old", old,
			func() error { return nil }, func() error { return nil })
	}()
	select {
	case err := <-delivered:
		t.Fatalf("old decision crossed Engine.Ensure publication: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseEnsure)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("old decision after replacement start = %v", err)
	}
}

func (e *fakeEngine) Name() string { return "fake" }
func (e *fakeEngine) Ensure(_ context.Context, spec engine.Spec) (engine.Instance, error) {
	if e.ensureBlock != nil {
		<-e.ensureBlock
	}
	e.mu.Lock()
	e.ensured = append(e.ensured, spec.Key)
	if e.ensureErr != nil {
		e.mu.Unlock()
		return nil, e.ensureErr
	}
	in, ok := e.insts[spec.Key]
	if !ok {
		in = &fakeInstance{key: spec.Key}
		e.insts[spec.Key] = in
	}
	hook := e.ensurePublished
	e.mu.Unlock()
	if hook != nil {
		hook(in)
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
	instance := e.insts[key]
	if instance != nil && !e.killRefuses {
		instance.mu.Lock()
		instance.dead = true
		instance.mu.Unlock()
		delete(e.insts, key)
	}
	hook := e.killObserved
	e.mu.Unlock()
	if hook != nil && instance != nil {
		hook(instance)
	}
}
func (e *fakeEngine) Shutdown() {}

// ensuredKeys copies the keys Ensure has been called with, under the lock, so a
// test can read them while a start is still in flight.
func (e *fakeEngine) ensuredKeys() []engine.Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]engine.Key(nil), e.ensured...)
}

// running registers an already-live agent pane for id.
func (e *fakeEngine) running(id string) *fakeInstance {
	e.mu.Lock()
	defer e.mu.Unlock()
	in := &fakeInstance{key: engine.Key{AgentID: id, Tab: panespec.TabAgent}}
	e.insts[in.key] = in
	return in
}

// steerDaemon is a daemon with a fake engine and an isolated store. A `prompt`
// to a stopped agent now starts it on its own goroutine, so the daemon is wired
// with a join point: without one the start outlives the test, and the isolated
// HOME is torn down under it.
func steerDaemon(t *testing.T) (*Daemon, *fakeEngine) {
	t.Helper()
	isolateHome(t)
	eng := newFakeEngine()
	d := New("", nil, time.Hour)
	d.engine = eng
	d.steerSettle = time.Millisecond
	d.agentsUnder = func(id string) ([]string, error) { return []string{id}, nil }
	d.launchSpec = testLaunchSpecResolver
	d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, []string{"sh"}, nil
	}
	d.steerStarted = make(chan string, 8)
	return d, eng
}

// waitStarted blocks until a deferred start finishes, failing the test if none
// does. Every test that prompts a stopped agent must call it before asserting on
// the engine — the point of the change under test is that steer returns first.
func waitStarted(t *testing.T, d *Daemon) {
	t.Helper()
	select {
	case <-d.steerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred start behind an accepted prompt never finished")
	}
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
			"\x1b[200~run the tests\x1b[201~\r"},
		{"claude interject", "claude",
			map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: "skip the flaky one"},
			"\x1b[200~skip the flaky one\x1b[201~\r"},
		{"claude stop", "claude",
			map[string]string{core.SteerVerb: core.SteerStop}, "\x03"},
		{"codex prompt", "codex",
			map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "hi"}, "hi\r"},
		{"codex stop", "codex",
			map[string]string{core.SteerVerb: core.SteerStop}, "\x1b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, eng := steerDaemon(t)
			putSession(t, "a1", tc.kind)
			in := eng.running("a1")
			// `stop` only fires mid-turn for a harness whose interrupt key is unsafe
			// at an idle prompt, so put the session in a turn.
			if tc.fields[core.SteerVerb] == core.SteerStop {
				markBusy(t, "a1")
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

// TestClaudePermissionObservationsCannotDriveThePane pins the fail-closed
// compatibility boundary: even a legacy journal row plus a current generation
// cannot turn session-controlled Claude telemetry into an answerable prompt.
func TestClaudePermissionObservationsCannotDriveThePane(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	generation, err := d.permissions.observe("a1", in)
	if err != nil {
		t.Fatal(err)
	}

	if err := core.AppendPermission(convID("a1"), core.PermissionRecord{RequestID: "reported", Tool: "Bash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.bindPermissionRequest("a1", "reported"); err == nil {
		t.Fatal("session-controlled Claude journal acquired an answerable binding")
	}
	err = d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerAllow,
			core.SteerRequestID:           "reported",
			access.RuntimeGenerationField: generation,
		},
	})
	if err == nil || !strings.Contains(err.Error(), `no pending request "reported"`) {
		t.Fatalf("Claude self observation permission = %v", err)
	}
	if got := in.written(); got != "" {
		t.Fatalf("nonanswerable Claude observation wrote %q to pane", got)
	}
}

// TestAttachedPaneStillDeliversManualClaudePermissionKey distinguishes the
// disabled remote permission verb from ordinary trusted-host pane interaction.
// A human attached to the pane can still press Claude's real prompt keys.
func TestAttachedPaneStillDeliversManualClaudePermissionKey(t *testing.T) {
	in := &fakeInstance{}
	client := &connState{panes: map[string]paneRoute{"pane": {inst: in}}}
	client.paneInput("pane", []byte("\r"))
	if got := in.written(); got != "\r" {
		t.Fatalf("attached pane input = %q, want manual Enter", got)
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
	markBusy(t, "a1")
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
// caller doesn't have to know whether the process happens to be running. The
// start is deferred past the ack now, so the assertions wait for it.
func TestSteerPromptStartsStoppedAgent(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")

	if err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1",
		Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
	}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	waitStarted(t, d)

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
	for time.Now().Before(deadline) && in.written() != "\x1b[200~go\x1b[201~\r" {
		time.Sleep(2 * time.Millisecond)
	}
	if got := in.written(); got != "\x1b[200~go\x1b[201~\r" {
		t.Fatalf("pane received %q, want %q", got, "\x1b[200~go\x1b[201~\r")
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
		if got := in.written(); got != "\x1b[200~add a repo\x1b[201~\r" {
			t.Fatalf("pane received %q, want %q", got, "\x1b[200~add a repo\x1b[201~\r")
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
		waitStarted(t, d)
		key := engine.Key{AgentID: console.ID, Tab: panespec.TabAgent}
		inst, ok := eng.Lookup(key)
		if !ok {
			t.Fatal("no console instance after a prompt to the stopped console")
		}
		in := inst.(*fakeInstance)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && in.written() != "\x1b[200~go\x1b[201~\r" {
			time.Sleep(2 * time.Millisecond)
		}
		if got := in.written(); got != "\x1b[200~go\x1b[201~\r" {
			t.Fatalf("pane received %q, want %q", got, "\x1b[200~go\x1b[201~\r")
		}
	})
}

// TestSteerAcksBeforeStarting is the point of the whole change: the verb is
// answered while the cold start is still in flight. The fake engine parks inside
// Ensure, so a steer that still waited for the start could not return — which is
// exactly the several-second stall that made a relayed prompt to an idle session
// time out at the web client (AGE-191).
func TestSteerAcksBeforeStarting(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	release := make(chan struct{})
	eng.ensureBlock = release

	acked := make(chan error, 1)
	go func() {
		acked <- d.steer(context.Background(), core.Action{
			Action: core.ActionSteer, ID: "a1",
			Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
		})
	}()

	select {
	case err := <-acked:
		if err != nil {
			t.Fatalf("steer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("steer blocked on the cold start instead of acking first")
	}
	// The ack came back before the engine was even done starting.
	if got := eng.ensuredKeys(); len(got) != 0 {
		t.Fatalf("engine already finished starting %v before the ack", got)
	}
	close(release)
	waitStarted(t, d)

	inst, ok := eng.Lookup(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
	if !ok {
		t.Fatal("no instance after the deferred start")
	}
	in := inst.(*fakeInstance)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && in.written() != "\x1b[200~go\x1b[201~\r" {
		time.Sleep(2 * time.Millisecond)
	}
	if got := in.written(); got != "\x1b[200~go\x1b[201~\r" {
		t.Fatalf("pane received %q, want the prompt to arrive after the ack", got)
	}
}

// TestSteerPromptReportsStartFailure keeps the start path honest. The verb has
// already been acknowledged by the time the start fails, so the failure cannot
// come back as a returned error any more — it has to reach the caller on the
// session's runtime-events stream, as an `error`-level notice. Silence here
// would leave an orchestrator waiting for a turn that is never coming.
func TestSteerPromptReportsStartFailure(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	eng.ensureErr = os.ErrPermission

	if err := d.steer(context.Background(), core.Action{
		Action: core.ActionSteer, ID: "a1",
		Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "go"},
	}); err != nil {
		t.Fatalf("steer should have acked before the start could fail: %v", err)
	}
	waitStarted(t, d)

	recs := core.ReadJournal("a1")
	if len(recs) != 2 {
		t.Fatalf("journal = %+v, want the starting notice and the failure", recs)
	}
	if recs[0].Level != core.JournalInfo || !strings.Contains(recs[0].Text, "starting agent") {
		t.Errorf("first line = %+v, want an info notice that the agent is starting", recs[0])
	}
	if recs[1].Level != core.JournalError || !strings.Contains(recs[1].Text, "start agent a1") {
		t.Errorf("second line = %+v, want an error naming the failed start", recs[1])
	}

	// And the journal is what the event stream reads, so the orchestrator sees the
	// failure as a notice rather than having to parse a log.
	evs := runtimeevents.MapJournalLine("claude", mustJSON(t, recs[1]))
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeNotice {
		t.Fatalf("journal error mapped to %+v, want one notice event", evs)
	}
	if !strings.Contains(string(evs[0].Payload), core.JournalError) {
		t.Errorf("notice payload %s should carry the error level", evs[0].Payload)
	}
}

// mustJSON re-marshals a journal record into the line the reader tails.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSteerDoesNotSurviveADeadPane guards the deferred write: an agent that
// exits between the start and the settle must not be typed into.
func TestSteerDoesNotSurviveADeadPane(t *testing.T) {
	d, _ := steerDaemon(t)
	in := &fakeInstance{dead: true}
	d.deferInput(in, []engine.InputStep{{Bytes: []byte("go")}, {Bytes: []byte("\r")}})
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

func TestSteerPasteSequence(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	for _, text := range []string{"first\nsecond", "next"} {
		if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: text}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(in.sequences) != 2 {
		t.Fatalf("got %d sequences, want two atomic prompts", len(in.sequences))
	}
	for i, text := range []string{"first\nsecond", "next"} {
		steps := in.sequences[i]
		if len(steps) != 2 || string(steps[0].Bytes) != "\x1b[200~"+text+"\x1b[201~" || string(steps[1].Bytes) != "\r" || steps[1].DelayBefore != 100*time.Millisecond {
			t.Fatalf("sequence %d: %#v", i, steps)
		}
	}
	for _, text := range []string{"x\x1b[201~y", "x\x1b[200~y"} {
		if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: text}}); err == nil {
			t.Fatal("accepted embedded paste delimiter")
		}
	}
	if len(in.sequences) != 2 {
		t.Fatal("refused prompt sent input")
	}
}

func TestSteerStartupDelayPrecedesLaterPrompt(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	// The first prompt cold-starts the agent and reserves the startup settle at the
	// head of the input FIFO; wait for that deferred start to finish so the second
	// prompt finds the agent already up and needs no settle of its own.
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "first"}}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, d)
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "second"}}); err != nil {
		t.Fatal(err)
	}
	inst, _ := eng.Lookup(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
	in := inst.(*fakeInstance)
	if len(in.sequences) != 2 || in.sequences[0][0].DelayBefore != d.steerSettle || in.sequences[1][0].DelayBefore != 0 {
		t.Fatalf("startup ordering: %#v", in.sequences)
	}
}

// TestSteerPromptStartsTheCoordinator pins what a prompt to a workgroup id means:
// it reaches the workgroup's coordinator (the root's own session), starting that
// one session when stopped — not the members, which is what `start <root>`
// means. A prompt to a repo name likewise reaches the repo's home session.
func TestSteerPromptStartsTheCoordinator(t *testing.T) {
	d, eng := steerDaemon(t)
	d.agentsUnder = wsops.AgentIDsUnder // the real root → members resolution
	coordinatorDir := store.CoordinatorDir("wg1")
	if err := os.MkdirAll(coordinatorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.Session{
		{ID: "wg1", Name: "payments", Scope: store.ScopeWork, Agent: "claude", Dir: coordinatorDir, Created: 1},
		{ID: "a1", RootID: "wg1", Agent: "claude", Dir: t.TempDir(), ClaudeID: convID("a1"), Created: 2},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutRepo(store.Repo{Name: "api", Source: "octo/api", GitDir: filepath.Join(t.TempDir(), "api.git")}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := wsops.EnsureRepoHome("api"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"wg1", "api"} {
		if err := d.steer(context.Background(), core.Action{
			Action: core.ActionSteer, ID: id,
			Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "status?"},
		}); err != nil {
			t.Fatalf("steer %s: %v", id, err)
		}
		waitStarted(t, d)
		if _, ok := eng.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent}); !ok {
			t.Fatalf("no %s instance after a prompt to it", id)
		}
	}
	for _, k := range eng.ensuredKeys() {
		if k.AgentID == "a1" {
			t.Fatal("a prompt to the workgroup started a member instead of the coordinator")
		}
	}
	// `start <root>` is still the members.
	if err := d.startEngineFor(context.Background(), "wg1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := eng.Lookup(engine.Key{AgentID: "a1", Tab: panespec.TabAgent}); !ok {
		t.Fatal("start <root> did not start its member")
	}
}
