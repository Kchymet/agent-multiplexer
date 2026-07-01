package local

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"amux/internal/engine"
)

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
