package local

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/engine"
)

// Restarting from an automation shell must not carry its output preferences
// into the fresh PTYs. A pane can still explicitly request those preferences.
func TestPaneColorEnvironment(t *testing.T) {
	for _, key := range []string{"NO_COLOR", "FORCE_COLOR", "CLICOLOR", "CLICOLOR_FORCE"} {
		t.Setenv(key, "0")
	}
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "dumb")
	t.Setenv("TMUX", "launcher")
	t.Setenv("COLORTERM", "truecolor")
	for _, tc := range []struct {
		name  string
		extra []string
	}{
		{"default", nil},
		{"explicit pane preferences", []string{"NO_COLOR=1", "FORCE_COLOR=0", "CLICOLOR=0", "CLICOLOR_FORCE=0", "TERM=vt100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			for _, e := range buildEnv(tc.extra) {
				key, value, _ := strings.Cut(e, "=")
				if _, exists := got[key]; exists {
					t.Fatalf("duplicate environment key %q", key)
				}
				got[key] = value
			}
			if len(tc.extra) == 0 {
				for _, key := range []string{"NO_COLOR", "FORCE_COLOR", "CLICOLOR", "CLICOLOR_FORCE"} {
					if _, exists := got[key]; exists {
						t.Errorf("pane inherited launcher override %s", key)
					}
				}
				if got["TERM"] != "xterm-256color" {
					t.Errorf("TERM = %q", got["TERM"])
				}
			} else {
				for _, e := range tc.extra {
					key, value, _ := strings.Cut(e, "=")
					if got[key] != value {
						t.Errorf("explicit %s lost: got %q", e, got[key])
					}
				}
			}
			if _, exists := got["TMUX"]; exists {
				t.Error("pane inherited launcher TMUX")
			}
			if got["COLORTERM"] != "truecolor" {
				t.Error("lost terminal color capability")
			}
		})
	}
}

// collector accumulates an instance's output for substring assertions.
type collector struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *collector) sink() engine.Sink {
	return engine.Sink{Output: func(b []byte) {
		c.mu.Lock()
		c.buf.Write(b)
		c.mu.Unlock()
	}}
}

func (c *collector) has(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Contains(c.buf.Bytes(), []byte(sub))
}

func waitFor(t *testing.T, c *collector, sub string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if c.has(sub) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in output", sub)
}

// A local instance fans output out to every subscriber, keeps running when a
// subscriber detaches (so a UI can close without stopping the agent), replays
// its scrollback to a reattaching subscriber, and dies on Kill.
func TestLocalEngineFanoutPersistenceReplay(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "a1", Tab: 0}
	// A long-lived process that emits a marker we can wait on.
	spec := engine.Spec{Key: key, Argv: []string{"sh", "-c", "printf XYZZY; sleep 30"}, Cols: 80, Rows: 24}

	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Two concurrent subscribers both see live output (fan-out).
	a, b := &collector{}, &collector{}
	cancelA := inst.Subscribe(a.sink())
	inst.Subscribe(b.sink())
	waitFor(t, a, "XYZZY", 3*time.Second)
	waitFor(t, b, "XYZZY", 3*time.Second)

	// Detaching a subscriber must not stop the agent.
	cancelA()
	if got, ok := eng.Lookup(key); !ok || !got.Alive() {
		t.Fatal("instance should stay alive after a subscriber detaches")
	}

	// A reattaching subscriber replays the scrollback (the marker arrived before
	// it subscribed, yet it still sees it).
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "XYZZY", time.Second)

	// Ensure is idempotent for a live key.
	if again, _ := eng.Ensure(context.Background(), spec); again != inst {
		t.Fatal("Ensure should return the existing live instance")
	}

	// Kill stops it and drops it from the table (so a later Ensure respawns).
	eng.Kill(key)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && inst.Alive() {
		time.Sleep(10 * time.Millisecond)
	}
	if inst.Alive() {
		t.Fatal("instance should be dead after Kill")
	}
	if _, ok := eng.Lookup(key); ok {
		t.Fatal("killed instance should be removed from the engine")
	}
}

// waitDead polls until the instance stops reporting Alive, or fails.
func waitDead(t *testing.T, in engine.Instance, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !in.Alive() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("instance still alive")
}

// A graceful kill sends SIGTERM and lets the process flush and exit on its own,
// so an agent (e.g. Claude) can write its session transcript before dying. Here
// the process traps SIGTERM, prints a marker, and exits — the marker proves it
// wasn't hard-killed, and the whole thing returns well within the grace period.
func TestKillIsGracefulOnSIGTERM(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "graceful", Tab: 0}
	spec := engine.Spec{
		Key:  key,
		Argv: []string{"sh", "-c", "trap 'printf FLUSHED; exit 0' TERM; printf READY; while :; do sleep 0.02; done"},
		Cols: 80, Rows: 24,
	}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "READY", 3*time.Second)

	start := time.Now()
	inst.(*instance).terminate(4 * time.Second)
	if elapsed := time.Since(start); elapsed >= 4*time.Second {
		t.Fatalf("graceful exit should return before the grace period, took %v", elapsed)
	}
	if inst.Alive() {
		t.Fatal("instance should be dead after terminate")
	}
	if !c.has("FLUSHED") {
		t.Fatal("process was not allowed to flush on SIGTERM (no FLUSHED marker)")
	}
}

// A process that ignores SIGTERM is escalated to SIGKILL after the grace period,
// so a wedged agent can't block shutdown forever.
func TestKillEscalatesToSIGKILL(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "stubborn", Tab: 0}
	spec := engine.Spec{
		Key:  key,
		Argv: []string{"sh", "-c", "trap '' TERM; printf READY; while :; do sleep 0.02; done"},
		Cols: 80, Rows: 24,
	}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "READY", 3*time.Second)

	const grace = 300 * time.Millisecond
	start := time.Now()
	inst.(*instance).terminate(grace)
	elapsed := time.Since(start)
	if elapsed < grace {
		t.Fatalf("should have waited the grace period before SIGKILL, took %v", elapsed)
	}
	if inst.Alive() {
		t.Fatal("instance should be dead after SIGKILL")
	}
}

// A graceful shutdown defers terminating a mid-turn (ActivityBusy) instance
// until its activity probe reports it safe, then terminates as usual. Here the
// probe reports Busy for a short while, then flips to Safe; the process traps
// SIGTERM and prints a marker, so we can assert termination didn't begin until
// after the flip.
func TestShutdownWaitsForIdle(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "busy", Tab: 0}
	spec := engine.Spec{
		Key:  key,
		Argv: []string{"sh", "-c", "trap 'exit 0' TERM; printf READY; while :; do sleep 0.02; done"},
		Cols: 80, Rows: 24,
	}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "READY", 3*time.Second)

	// Busy for the first ~150ms, then safe.
	var safeAt time.Time
	const busyFor = 150 * time.Millisecond
	inst.(*instance).activity = func(engine.Key) engine.Activity {
		if safeAt.IsZero() {
			safeAt = time.Now().Add(busyFor)
		}
		if time.Now().Before(safeAt) {
			return engine.ActivityBusy
		}
		return engine.ActivitySafe
	}

	start := time.Now()
	// Generous idle budget (it should return well before it), short kill grace.
	inst.(*instance).shutdownWith(2*time.Second, 300*time.Millisecond)
	waited := time.Since(start)
	if inst.Alive() {
		t.Fatal("instance should be dead after shutdown")
	}
	// It must have waited out the busy window before terminating, not stopped
	// immediately.
	if waited < busyFor {
		t.Fatalf("shutdown terminated after %v; should have waited for the busy window (~%v)", waited, busyFor)
	}
}

// A shutdown wait is bounded: an instance stuck ActivityBusy forever is still
// terminated once the idle budget elapses, so a wedged turn can't block shutdown.
func TestShutdownIdleWaitIsBounded(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "stuck-busy", Tab: 0}
	spec := engine.Spec{
		Key:  key,
		Argv: []string{"sh", "-c", "trap 'exit 0' TERM; printf READY; while :; do sleep 0.02; done"},
		Cols: 80, Rows: 24,
	}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "READY", 3*time.Second)

	inst.(*instance).activity = func(engine.Key) engine.Activity { return engine.ActivityBusy }

	const idleBudget = 200 * time.Millisecond
	start := time.Now()
	inst.(*instance).shutdownWith(idleBudget, 300*time.Millisecond)
	waited := time.Since(start)
	if inst.Alive() {
		t.Fatal("instance should be dead after a bounded shutdown wait")
	}
	if waited < idleBudget {
		t.Fatalf("shutdown returned after %v; should have waited out the idle budget (%v)", waited, idleBudget)
	}
}

// Shutdown terminates instances in parallel, so its total cost is ~one grace
// period even with several stubborn (SIGTERM-ignoring) agents, not N of them.
func TestShutdownTerminatesInParallel(t *testing.T) {
	t.Setenv("AMUX_KILL_GRACE", "300ms")
	eng := New()

	const n = 4
	for i := 0; i < n; i++ {
		spec := engine.Spec{
			Key:  engine.Key{AgentID: "p", Tab: i},
			Argv: []string{"sh", "-c", "trap '' TERM; printf READY; while :; do sleep 0.02; done"},
			Cols: 80, Rows: 24,
		}
		inst, err := eng.Ensure(context.Background(), spec)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		c := &collector{}
		inst.Subscribe(c.sink())
		waitFor(t, c, "READY", 3*time.Second)
	}

	start := time.Now()
	eng.Shutdown()
	if elapsed := time.Since(start); elapsed > 2*300*time.Millisecond {
		t.Fatalf("parallel shutdown of %d instances took %v; expected ~one grace period", n, elapsed)
	}
	for _, k := range eng.Live() {
		t.Fatalf("instance %v still live after Shutdown", k)
	}
}

// A child that never drains its stdin eventually backs up the PTY input buffer,
// so ptmx.Write blocks. Input must not block its caller (the daemon serve loop),
// and inputLoop must not hold in.mu across that stalled write — otherwise pump,
// Resize, and Alive would wedge too. This is the daemon-side half of the freeze
// fix: before the per-instance input queue, Input wrote under in.mu and stalled
// the whole serve loop when the child stopped reading.
func TestInputNeverBlocksWhenChildIgnoresStdin(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "stuck", Tab: 0}
	spec := engine.Spec{Key: key, Argv: []string{"sh", "-c", "sleep 30"}, Cols: 80, Rows: 24}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Push far more than the PTY buffer can hold to guarantee a blocking write.
	chunk := bytes.Repeat([]byte("x"), 4096)
	sent := make(chan struct{})
	go func() {
		for i := 0; i < 2048; i++ {
			inst.Input(chunk)
		}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(3 * time.Second):
		t.Fatal("Input blocked while the child ignored its stdin")
	}

	// in.mu must stay free even while inputLoop is parked in a blocking ptmx.Write:
	// Alive() takes in.mu, so it answering promptly proves the lock isn't held
	// across the write.
	ans := make(chan bool, 1)
	go func() { ans <- inst.Alive() }()
	select {
	case <-ans:
	case <-time.After(2 * time.Second):
		t.Fatal("Alive() blocked — inputLoop is holding in.mu across a blocking write")
	}
}

// Input written to an instance reaches the process and its echo streams back.
func TestLocalEngineInput(t *testing.T) {
	eng := New()
	defer eng.Shutdown()

	key := engine.Key{AgentID: "a2", Tab: 2}
	spec := engine.Spec{Key: key, Argv: []string{"cat"}, Cols: 80, Rows: 24}
	inst, err := eng.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	inst.Input([]byte("ping\n"))
	waitFor(t, c, "ping", 3*time.Second)
}

// A real PTY reader observes the separation after a write, and later input may
// not sneak into the gap. This catches implementations that sleep in the caller
// or queue text and submit as unrelated entries.
func TestInputSequenceSeparatesWritesAndPreservesOrder(t *testing.T) {
	eng := New()
	defer eng.Shutdown()
	inst, err := eng.Ensure(context.Background(), engine.Spec{Key: engine.Key{AgentID: "sequence"}, Argv: []string{"sh", "-c", "stty raw -echo; printf READY; cat"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &collector{}
	inst.Subscribe(c.sink())
	waitFor(t, c, "READY", 3*time.Second)
	start := time.Now()
	inst.InputSequence([]engine.InputStep{{Bytes: []byte("PASTE")}, {Bytes: []byte("SUBMIT"), DelayBefore: 200 * time.Millisecond}})
	inst.Input([]byte("LATER"))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("queuing input blocked for the submit delay")
	}
	waitFor(t, c, "PASTE", 3*time.Second)
	waitFor(t, c, "PASTESUBMITLATER", 3*time.Second)
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("submit reached PTY without the requested delay")
	}
}

func TestInputSequenceCopiesAndDropsAtomically(t *testing.T) {
	in := &instance{inCh: make(chan []engine.InputStep, 1), inDone: make(chan struct{})}
	text := []byte("paste")
	steps := []engine.InputStep{{Bytes: text}, {Bytes: []byte("submit"), DelayBefore: time.Second}}
	in.InputSequence(steps)
	text[0] = 'X'
	steps[1].DelayBefore = 0
	in.InputSequence([]engine.InputStep{{Bytes: []byte("dropped")}, {Bytes: []byte("also dropped")}})
	got := <-in.inCh
	if len(got) != 2 || string(got[0].Bytes) != "paste" || string(got[1].Bytes) != "submit" || got[1].DelayBefore != time.Second {
		t.Fatalf("queued sequence changed: %#v", got)
	}
	if len(in.inCh) != 0 {
		t.Fatal("full queue retained part of the next sequence")
	}
	close(in.inDone)
	in.InputSequence([]engine.InputStep{{Bytes: []byte("dead")}})
	if len(in.inCh) != 0 {
		t.Fatal("dead instance accepted input")
	}
}

func TestInputSequenceDelayCancelsOnExit(t *testing.T) {
	in := &instance{inCh: make(chan []engine.InputStep, 1), inDone: make(chan struct{})}
	in.InputSequence([]engine.InputStep{{Bytes: []byte("never"), DelayBefore: time.Hour}})
	done := make(chan struct{})
	go func() { in.inputLoop(); close(done) }()
	deadline := time.Now().Add(time.Second)
	for len(in.inCh) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(in.inCh) > 0 {
		t.Fatal("writer did not start the delayed sequence")
	}
	close(in.inDone)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("input writer survived instance shutdown")
	}
}

// Delay must begin after a blocked write drains. A timer in the daemon would
// expire while that write was blocked and let paste plus submit arrive together.
func TestInputSequenceDelayStartsAfterBlockedWrite(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	in := &instance{ptmx: writer, inCh: make(chan []engine.InputStep, 1), inDone: make(chan struct{})}
	defer close(in.inDone)
	text := bytes.Repeat([]byte("p"), 1<<20)
	in.InputSequence([]engine.InputStep{{Bytes: text}, {Bytes: []byte("!"), DelayBefore: 100 * time.Millisecond}})
	go in.inputLoop()
	// This pipe cannot hold the first write, and nobody is reading it yet.
	time.Sleep(200 * time.Millisecond)
	if _, err := io.CopyN(io.Discard, reader, int64(len(text))); err != nil {
		t.Fatal(err)
	}
	drained := time.Now()
	if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var submit [1]byte
	if _, err := io.ReadFull(reader, submit[:]); err != nil {
		t.Fatal(err)
	}
	if submit[0] != '!' || time.Since(drained) < 80*time.Millisecond {
		t.Fatal("submit delay elapsed while paste write was blocked")
	}
}
