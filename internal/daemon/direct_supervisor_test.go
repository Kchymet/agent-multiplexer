package daemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/panespec"
	"amux/internal/source"
)

// Only the production daemon supervises a direct listener; New (tests,
// embedded daemons) never does.
func TestNewDoesNotSuperviseDirect(t *testing.T) {
	if d := New("", nil, time.Hour); d.directSupervisor != nil {
		t.Fatal("New wired a direct supervisor")
	}
}

// The supervisor starts once the daemon is serving and is joined during
// graceful shutdown before the engine stops, so the managed listener never
// outlives the daemon that owns it.
func TestRunSupervisesDirectListenerUntilShutdown(t *testing.T) {
	isolateControl(t)
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	pollSource := &lifecycleBlockingSource{entered: make(chan struct{}), exited: make(chan struct{})}
	eng := newLifecycleTestEngine()
	d := New("", []source.Source{pollSource}, time.Hour)
	d.authority = authority
	d.engine = eng
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) { return listener, func() {}, nil }
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	d.directSupervisor = func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-started:
	case err := <-runDone:
		t.Fatalf("Run exited before supervising: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("direct supervisor not started")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run shutdown deadlocked")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("daemon returned without stopping the direct supervisor")
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
}
