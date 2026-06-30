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
