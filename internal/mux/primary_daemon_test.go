package mux

import (
	"context"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend/hostbackendtest"
)

// The production Primary is the shared host backend: snapshot, dispatch and
// panes each authenticate their own daemon connection through it.
func TestDaemonPrimaryRoutesThroughHostBackend(t *testing.T) {
	snap, verb, pane := hostbackendtest.New("snap"), hostbackendtest.New("verb"), hostbackendtest.New("pane")
	snap.SnapshotFn = func() ([]core.Session, error) { return []core.Session{{ID: "w1"}}, nil }
	verb.DispatchFn = func(a core.Action) (string, error) { return "made:" + a.Action, nil }
	d := hostbackendtest.NewDialer(snap, verb, pane)
	var p Primary = daemonPrimary{backend: d.Backend()}
	ctx := context.Background()

	if s, err := p.Snapshot(ctx); err != nil || len(s) != 1 || s[0].ID != "w1" {
		t.Fatalf("Snapshot = %v, %v", s, err)
	}
	if id, err := p.Dispatch(ctx, core.Action{Action: core.ActionStart}); err != nil || id != "made:"+core.ActionStart {
		t.Fatalf("Dispatch = %q, %v", id, err)
	}
	relay, err := p.OpenPane(ctx, PaneRequest{Agent: "a1", Tab: 2, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	pane.Frames <- daemon.Frame{Pane: &core.PaneFrame{PaneID: "relay-pane", Data: []byte("hi")}}
	if f, err := relay.Next(ctx); err != nil || string(f.Data) != "hi" {
		t.Fatalf("pane Next = %+v, %v", f, err)
	}
	if err := relay.Close(); err != nil || !pane.IsClosed() {
		t.Fatalf("pane Close = %v (closed=%v)", err, pane.IsClosed())
	}
	if d.Dials() != 3 || !snap.IsClosed() || !verb.IsClosed() {
		t.Fatalf("dials=%d snapClosed=%v verbClosed=%v", d.Dials(), snap.IsClosed(), verb.IsClosed())
	}
}
