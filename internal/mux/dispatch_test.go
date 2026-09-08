package mux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/muxproto"
	"amux/internal/panespec"
)

func attachedClient(s *Server) *client {
	ctx, cancel := context.WithCancel(context.Background())
	cl := &client{
		out: make(chan muxproto.ServerMsg, 8), done: make(chan struct{}),
		panes: map[string]*route{}, obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1),
		server: s, ctx: ctx, cancel: cancel, epoch: s.epoch,
	}
	s.clients[cl] = true
	return cl
}

func waitRoute(t *testing.T, s *Server, cl *client, paneID string) *route {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		r := cl.panes[paneID]
		ready := r != nil && r.relay != nil
		s.mu.Unlock()
		if ready {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pane %q did not attach", paneID)
	return nil
}

func TestSnapshotRemovalClosesOnlyRemovedPrimaryPane(t *testing.T) {
	var mu sync.Mutex
	relays := map[string]*testPaneRelay{}
	sessions := []core.Session{{ID: "A"}, {ID: "B"}}
	s := New(testPrimary{
		snapshot: func(context.Context) ([]core.Session, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]core.Session(nil), sessions...), nil
		},
		openPane: func(_ context.Context, request PaneRequest) (PaneRelay, error) {
			r := newTestPaneRelay()
			mu.Lock()
			relays[request.Agent] = r
			mu.Unlock()
			return r, nil
		},
	})
	cl := attachedClient(s)
	s.remember(sessions)
	s.openPane(cl, muxproto.ClientMsg{PaneID: "pa", Agent: "A", Tab: panespec.TabAgent})
	s.openPane(cl, muxproto.ClientMsg{PaneID: "pb", Agent: "B", Tab: panespec.TabAgent})
	waitRoute(t, s, cl, "pa")
	waitRoute(t, s, cl, "pb")

	mu.Lock()
	sessions = []core.Session{{ID: "A", Archived: true}, {ID: "B"}}
	a, b := relays["A"], relays["B"]
	mu.Unlock()
	s.broadcast(context.Background())

	select {
	case <-a.closed:
	case <-time.After(time.Second):
		t.Fatal("archived target retained its primary pane relay")
	}
	select {
	case <-b.closed:
		t.Fatal("unrelated active target was revoked")
	default:
	}
	if s.clientRoute(cl, "pa") != nil || s.clientRoute(cl, "pb") == nil {
		t.Fatalf("routes after archive: %+v", cl.panes)
	}
	s.dropClient(cl)
}

func TestPrimarySuspensionCancelsInFlightPaneOpen(t *testing.T) {
	openEntered := make(chan struct{})
	s := New(testPrimary{openPane: func(ctx context.Context, _ PaneRequest) (PaneRelay, error) {
		close(openEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "a1"}})
	done := make(chan struct{})
	go func() {
		s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
		close(done)
	}()
	<-openEntered
	s.suspend()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("suspend did not cancel/join in-flight primary pane admission")
	}
	if len(s.routes) != 0 || len(cl.panes) != 0 {
		t.Fatalf("stale pane open retained routes: server=%v client=%v", s.routes, cl.panes)
	}
}

func TestUnpublishedPaneTargetNeverReachesPrimary(t *testing.T) {
	called := false
	s := New(testPrimary{openPane: func(context.Context, PaneRequest) (PaneRelay, error) {
		called = true
		return newTestPaneRelay(), nil
	}})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "published"}, {ID: "archived", Archived: true}})
	for _, target := range []string{"unknown", "archived", "/tmp/alias"} {
		s.openPane(cl, muxproto.ClientMsg{PaneID: "p-" + target, Agent: target, Tab: panespec.TabAgent})
	}
	if called {
		t.Fatal("unpublished/path-shaped pane target reached primary daemon")
	}
	if len(s.routes) != 0 || len(cl.panes) != 0 {
		t.Fatalf("unpublished targets retained routes: %v %v", s.routes, cl.panes)
	}
}

func TestTargetRemovalCancelsInFlightPaneOpen(t *testing.T) {
	entered := make(chan struct{})
	s := New(testPrimary{
		snapshot: func(context.Context) ([]core.Session, error) { return nil, nil },
		openPane: func(ctx context.Context, _ PaneRequest) (PaneRelay, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "a1"}})
	done := make(chan struct{})
	go func() {
		s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
		close(done)
	}()
	<-entered
	s.broadcast(context.Background())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("target removal did not cancel/join in-flight pane open")
	}
	if len(s.routes) != 0 || len(cl.panes) != 0 {
		t.Fatalf("removed target's stale open retained routes: %v %v", s.routes, cl.panes)
	}
}

func TestPrimaryPollFailureRevokesClientAndPaneRelay(t *testing.T) {
	relay := newTestPaneRelay()
	s := New(testPrimary{
		snapshot: func(context.Context) ([]core.Session, error) {
			return nil, errors.New("primary unavailable")
		},
		openPane: func(context.Context, PaneRequest) (PaneRelay, error) { return relay, nil },
	})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "a1"}})
	s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
	waitRoute(t, s, cl, "p1")

	s.broadcast(context.Background())
	select {
	case <-cl.done:
	case <-time.After(time.Second):
		t.Fatal("client retained after primary poll failure")
	}
	select {
	case <-relay.closed:
	case <-time.After(time.Second):
		t.Fatal("primary pane relay retained after poll failure")
	}
	if len(s.sessions()) != 0 || len(s.routes) != 0 {
		t.Fatal("authority state retained after primary poll failure")
	}
}

func TestSuccessfulActionDoesNotLocallyOwnRuntimeStop(t *testing.T) {
	relay := newTestPaneRelay()
	removed := false
	s := New(testPrimary{
		dispatch: func(_ context.Context, got core.Action) (string, error) {
			if got.Action != core.ActionArchive || got.ID != "a1" {
				t.Fatalf("dispatch = %+v", got)
			}
			removed = true
			return "", nil
		},
		snapshot: func(context.Context) ([]core.Session, error) {
			if removed {
				return nil, nil
			}
			return []core.Session{{ID: "a1"}}, nil
		},
		openPane: func(context.Context, PaneRequest) (PaneRelay, error) { return relay, nil },
	})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "a1"}})
	s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
	waitRoute(t, s, cl, "p1")
	s.handleMsg(cl, muxproto.ClientMsg{Type: muxproto.CAction, Action: core.ActionArchive, ID: "a1"})
	select {
	case <-relay.closed:
		t.Fatal("mux locally stopped a primary-owned runtime before authoritative reconciliation")
	default:
	}
	s.broadcast(context.Background())
	select {
	case <-relay.closed:
	case <-time.After(time.Second):
		t.Fatal("removed target retained relay after authoritative snapshot")
	}
}

func TestServeShutdownClosesAndJoinsPrimaryPaneRelay(t *testing.T) {
	relay := newTestPaneRelay()
	s := New(testPrimary{openPane: func(context.Context, PaneRequest) (PaneRelay, error) { return relay, nil }})
	cl := attachedClient(s)
	s.remember([]core.Session{{ID: "a1"}})
	s.openPane(cl, muxproto.ClientMsg{PaneID: "p1", Agent: "a1", Tab: panespec.TabAgent})
	r := waitRoute(t, s, cl, "p1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not join its relay work on cancellation")
	}
	select {
	case <-relay.closed:
	case <-time.After(time.Second):
		t.Fatal("primary pane relay survived Serve")
	}
	select {
	case <-r.done:
	default:
		t.Fatal("primary pane pump was not joined before Serve returned")
	}
}

func TestSuspendCancelsAndJoinsActiveAction(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	s := New(testPrimary{dispatch: func(ctx context.Context, _ core.Action) (string, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return "", ctx.Err()
	}})
	cl := attachedClient(s)

	handled := make(chan struct{})
	go func() {
		s.handleMsg(cl, muxproto.ClientMsg{Type: muxproto.CAction, Action: core.ActionRefresh})
		close(handled)
	}()
	<-entered

	suspended := make(chan struct{})
	go func() {
		s.suspend()
		close(suspended)
	}()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("suspension did not cancel the active primary action")
	}
	select {
	case <-suspended:
		t.Fatal("suspension returned before the canceled primary action exited")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("active action handler did not exit after cancellation")
	}
	select {
	case <-suspended:
	case <-time.After(time.Second):
		t.Fatal("suspension did not join the active action handler")
	}
}

func TestJoinPrimaryCallWaitsForCallbackAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	closed := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := joinPrimaryCall(ctx, func() { close(closed) }, func() (string, error) {
			close(entered)
			<-release
			return "committed", nil
		})
		result <- err
	}()
	<-entered
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the primary transport")
	}
	select {
	case err := <-result:
		t.Fatalf("primary call returned before its callback exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled primary call error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("primary call did not join its callback after transport close")
	}
}
