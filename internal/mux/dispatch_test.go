package mux

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/muxproto"
	"amux/internal/panespec"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestKillPanesForKillsOnlyTargetAgent verifies the mux-server side of the
// AGE-132 fix: a StopsEngine verb (delete/archive) tears down exactly the target
// agent's live panes — so its PTY-backed process doesn't leak — and leaves other
// agents' panes running. Before, the mux server never killed a pane on a
// lifecycle action, so a delete left the harness process alive.
func TestKillPanesForKillsOnlyTargetAgent(t *testing.T) {
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

	s.killPanesFor([]string{"A"})

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

func TestPrimarySuspensionBlocksInFlightPaneSpawn(t *testing.T) {
	launchEntered := make(chan struct{})
	releaseLaunch := make(chan struct{})
	s := New(testPrimary{launch: func(context.Context, string) (panespec.LaunchSpec, error) {
		close(launchEntered)
		<-releaseLaunch
		return panespec.LaunchSpec{}, nil
	}})
	cl := &client{out: make(chan muxproto.ServerMsg, 4), done: make(chan struct{}), panes: map[string]string{}, obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1)}
	s.clients[cl] = true
	resolved := make(chan struct{}, 1)
	s.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		resolved <- struct{}{}
		return "", nil, []string{"forbidden"}, nil
	}
	done := make(chan struct{})
	go func() {
		s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
		close(done)
	}()
	<-launchEntered
	s.suspend()
	close(releaseLaunch)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight pane open did not finish after suspension")
	}
	select {
	case <-resolved:
		t.Fatal("suspended pane open reached process resolution")
	default:
	}
	if len(s.routes) != 0 || len(cl.panes) != 0 {
		t.Fatalf("suspended pane open retained routes: server=%v client=%v", s.routes, cl.panes)
	}
}

func TestStopsEngineWaitsForPrimarySuccessAndUsesAuthoritativeSnapshot(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	fail := true
	primary := testPrimary{dispatch: func(_ context.Context, got core.Action) (string, error) {
		if got.Action != core.ActionArchive || got.ID != "root" {
			t.Fatalf("dispatch = %+v", got)
		}
		if fail {
			return "", errors.New("denied")
		}
		return "", nil
	}}
	s := New(primary)
	s.hconn = harnessproto.NewConn(a)
	s.remember([]core.Session{{ID: "root", IsRoot: true}, {ID: "a1", RootID: "root"}, {ID: "other", RootID: "else"}})
	s.routes = map[string]route{
		"h-root":  {agent: "root"},
		"h-child": {agent: "a1"},
		"h-other": {agent: "other"},
	}
	cl := &client{out: make(chan muxproto.ServerMsg, 4), done: make(chan struct{}), panes: map[string]string{}, obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1)}
	s.clients[cl] = true
	hc := harnessproto.NewConn(b)
	killed := make(chan string, 4)
	go func() {
		for {
			m, err := hc.ReadMux()
			if err != nil {
				return
			}
			if m.Type == harnessproto.MKill {
				killed <- m.PaneID
			}
		}
	}()

	s.handleMsg(cl, muxproto.ClientMsg{Type: muxproto.CAction, Action: core.ActionArchive, ID: "root"})
	select {
	case pane := <-killed:
		t.Fatalf("failed primary action killed %q", pane)
	case <-time.After(50 * time.Millisecond):
	}
	fail = false
	s.handleMsg(cl, muxproto.ClientMsg{Type: muxproto.CAction, Action: core.ActionArchive, ID: "root"})
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case pane := <-killed:
			got[pane] = true
		case <-time.After(time.Second):
			t.Fatalf("kills = %v, want root and child", got)
		}
	}
	if !got["h-root"] || !got["h-child"] || got["h-other"] {
		t.Fatalf("kills = %v, want h-root/h-child only", got)
	}
}

func TestPrimaryPollFailureRevokesClientsAndMuxPanes(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := New(testPrimary{snapshot: func(context.Context) ([]core.Session, error) {
		return nil, errors.New("primary unavailable")
	}})
	s.hconn = harnessproto.NewConn(a)
	cl := &client{done: make(chan struct{}), panes: map[string]string{"p1": "h1"}, obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1)}
	s.clients[cl] = true
	s.routes["h1"] = route{cl: cl, clientPane: "p1", agent: "a1"}
	s.remember([]core.Session{{ID: "a1"}})

	killed := make(chan harnessproto.MuxMsg, 1)
	go func() {
		m, _ := harnessproto.NewConn(b).ReadMux()
		killed <- m
	}()
	s.broadcast(context.Background())
	select {
	case <-cl.done:
	case <-time.After(time.Second):
		t.Fatal("client retained after primary poll failure")
	}
	select {
	case m := <-killed:
		if m.Type != harnessproto.MKill || m.PaneID != "h1" {
			t.Fatalf("kill = %+v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("mux pane retained after primary poll failure")
	}
	if len(s.sessions()) != 0 {
		t.Fatal("authoritative snapshot retained after primary poll failure")
	}
}
