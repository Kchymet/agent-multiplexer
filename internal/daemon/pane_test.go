package daemon

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
)

// testDaemon is a daemon with no sources, a real local engine, and a fake spec
// resolver that runs a trivial marker-emitting process instead of a real agent.
func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := New("", nil, time.Hour)
	d.engine = local.New()
	t.Cleanup(d.engine.Shutdown)
	d.resolve = func(agentID string, tab int) (string, []string, []string, error) {
		return "", nil, []string{"sh", "-c", "printf MARKER; sleep 30"}, nil
	}
	d.agentsUnder = func(id string) ([]string, error) { return []string{id}, nil }
	return d
}

// dialDaemon connects a client to a fresh serve goroutine over an in-memory pipe.
func dialDaemon(t *testing.T, d *Daemon) (*Client, func()) {
	t.Helper()
	srv, cli := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.serve(ctx, srv); close(done) }()
	c := newClient(cli)
	return c, func() {
		_ = cli.Close()
		cancel()
		<-done
	}
}

// readPaneMarker reads frames until a pane.output for paneID contains marker.
func readPaneMarker(t *testing.T, c *Client, paneID, marker string, d time.Duration) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		f, err := c.Next()
		if err != nil {
			t.Fatalf("waiting for %q: %v", marker, err)
		}
		if f.Pane != nil && f.Pane.PaneID == paneID && f.Pane.Type == core.FramePaneOutput &&
			bytes.Contains(f.Pane.Data, []byte(marker)) {
			return
		}
	}
}

// Detaching a UI (closing the connection) must NOT stop the agent: the engine
// instance keeps running, and a fresh connection reattaching to the same pane
// replays the scrollback.
func TestPaneSurvivesDisconnectAndReplays(t *testing.T) {
	d := testDaemon(t)
	key := engine.Key{AgentID: "a1", Tab: 0}

	c1, close1 := dialDaemon(t, d)
	if err := c1.PaneOpen("p1", "a1", 0, 80, 24); err != nil {
		t.Fatalf("PaneOpen: %v", err)
	}
	readPaneMarker(t, c1, "p1", "MARKER", 3*time.Second)

	if inst, ok := d.engine.Lookup(key); !ok || !inst.Alive() {
		t.Fatal("agent should be live after open")
	}

	// Simulate the UI quitting.
	close1()

	// The agent must still be running a moment later (detach != kill).
	time.Sleep(100 * time.Millisecond)
	if inst, ok := d.engine.Lookup(key); !ok || !inst.Alive() {
		t.Fatal("agent must keep running after the UI disconnects")
	}

	// A new UI reattaches and repaints from the replayed scrollback.
	c2, close2 := dialDaemon(t, d)
	defer close2()
	if err := c2.PaneOpen("p1", "a1", 0, 80, 24); err != nil {
		t.Fatalf("reattach PaneOpen: %v", err)
	}
	readPaneMarker(t, c2, "p1", "MARKER", 3*time.Second)
}

// The "start" action launches an agent's process in the engine with no UI
// attached — the way a CLI-created session comes up running. A later pane.open
// reattaches to that same instance rather than spawning a second one.
func TestStartActionLaunchesAgentHeadless(t *testing.T) {
	d := testDaemon(t)
	key := engine.Key{AgentID: "a1", Tab: 0}

	c, closer := dialDaemon(t, d)
	defer closer()

	if err := c.Send(core.Action{Action: core.ActionStart, ID: "a1"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The result frame confirms the action ran; the process is now live in the
	// engine even though nothing subscribed to its output.
	waitResult(t, c, 3*time.Second)

	inst, ok := d.engine.Lookup(key)
	if !ok || !inst.Alive() {
		t.Fatal("agent should be live in the engine after start, with no pane open")
	}

	// Opening a pane reattaches to the running instance (idempotent Ensure) and
	// replays its scrollback — same instance, not a fresh spawn.
	if err := c.PaneOpen("p1", "a1", 0, 80, 24); err != nil {
		t.Fatalf("PaneOpen: %v", err)
	}
	readPaneMarker(t, c, "p1", "MARKER", 3*time.Second)
	if inst2, ok := d.engine.Lookup(key); !ok || inst2 != inst {
		t.Fatal("pane.open after start should reuse the same instance")
	}
}

// waitResult reads frames until an action result arrives, failing on a not-ok one.
func waitResult(t *testing.T, c *Client, d time.Duration) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	defer c.conn.SetReadDeadline(time.Time{})
	for {
		f, err := c.Next()
		if err != nil {
			t.Fatalf("waiting for result: %v", err)
		}
		if f.Result != nil {
			if !f.Result.OK {
				t.Fatalf("action failed: %s", f.Result.Error)
			}
			return
		}
	}
}

// Deleting an agent stops its engine instance (the process actually ends).
func TestDeleteKillsEngineInstance(t *testing.T) {
	d := testDaemon(t)
	key := engine.Key{AgentID: "a1", Tab: 0}

	c, closer := dialDaemon(t, d)
	defer closer()
	if err := c.PaneOpen("p1", "a1", 0, 80, 24); err != nil {
		t.Fatalf("PaneOpen: %v", err)
	}
	readPaneMarker(t, c, "p1", "MARKER", 3*time.Second)

	// Seed the snapshot so killEngineFor can see the agent, then kill it.
	d.mu.Lock()
	d.sessions = []core.Session{{ID: "a1"}}
	d.mu.Unlock()
	d.killEngineFor("a1")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := d.engine.Lookup(key); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("engine instance should be gone after delete")
}
