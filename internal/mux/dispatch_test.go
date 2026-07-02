package mux

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestKillPanesForKillsOnlyTargetAgent verifies the mux-server side of the
// AGE-132 fix: a StopsEngine verb (delete/archive) tears down exactly the target
// agent's live panes — so its PTY-backed process doesn't leak — and leaves other
// agents' panes running. Before, the mux server never killed a pane on a
// lifecycle action, so a delete left the harness process alive.
func TestKillPanesForKillsOnlyTargetAgent(t *testing.T) {
	// Isolate the store so wsops.AgentIDsUnder falls back to the id itself (the
	// agent isn't a persisted workgroup root here).
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	s := New()
	s.hconn = harnessproto.NewConn(a)
	s.routes = map[string]route{
		"h1": {agent: "A", clientPane: "p1"},
		"h2": {agent: "A", clientPane: "p2"},
		"h3": {agent: "B", clientPane: "p3"},
	}

	// Drain MKill frames from the harness side of the pipe.
	killed := make(chan string, 8)
	go func() {
		hconn := harnessproto.NewConn(b)
		for {
			m, err := hconn.ReadMux()
			if err != nil {
				return
			}
			if m.Type == harnessproto.MKill {
				killed <- m.PaneID
			}
		}
	}()

	s.killPanesFor("A")

	// Both of agent A's panes are killed (order-independent).
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case p := <-killed:
			got[p] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; killed so far: %v", got)
		}
	}
	if !got["h1"] || !got["h2"] {
		t.Errorf("killed = %v, want h1 and h2", got)
	}
	// Agent B's pane must NOT be killed — no further frame arrives.
	select {
	case p := <-killed:
		t.Errorf("unexpected kill of %q (belongs to agent B)", p)
	case <-time.After(200 * time.Millisecond):
	}
}
