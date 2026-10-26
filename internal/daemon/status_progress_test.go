package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/daemonstatus"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/panespec"
	"amux/internal/source"
)

// progressTestEngine reports one busy instance through the shutdown observer
// and records what the status file said at each step.
type progressTestEngine struct {
	*lifecycleTestEngine
	observe func([]local.ShutdownProgress)
	seen    []daemonstatus.Status
}

func (e *progressTestEngine) SetShutdownObserver(f func([]local.ShutdownProgress)) { e.observe = f }

func (e *progressTestEngine) Shutdown() {
	if e.observe != nil {
		key := engine.Key{AgentID: "agent-1"}
		e.observe([]local.ShutdownProgress{{Key: key, State: local.ShutdownBusy, Until: time.Now().Add(time.Minute)}})
		s, _ := daemonstatus.Read(daemonstatus.Path())
		e.seen = append(e.seen, s)
		e.observe([]local.ShutdownProgress{{Key: key, State: local.ShutdownStopped}})
	}
	e.lifecycleTestEngine.Shutdown()
}

type titledSource struct{}

func (titledSource) Name() string { return "titled" }
func (titledSource) Poll(context.Context) ([]core.Session, error) {
	return []core.Session{{ID: "agent-1", Title: "fix login"}}, nil
}

// Startup publishes its phase until the listener is up, then clears the file;
// shutdown republishes it with the agents the engine is waiting on (titled from
// the last poll) and removes it before the singleton lock is released.
func TestRunPublishesLifecycleStatus(t *testing.T) {
	isolateControl(t)
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.EnsureHost(context.Background()); err != nil {
		t.Fatal(err)
	}
	eng := &progressTestEngine{lifecycleTestEngine: newLifecycleTestEngine()}
	d := New("", []source.Source{titledSource{}}, time.Hour)
	d.authority = authority
	d.engine = eng
	listener := newLifecycleListener()
	var atListen daemonstatus.Status
	listening := make(chan struct{})
	d.listen = func(string, string) (net.Listener, func(), error) {
		atListen, _ = daemonstatus.Read(daemonstatus.Path())
		close(listening)
		return listener, func() {}, nil
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in status test")
	}
	eng.check = func() error {
		s, ok := daemonstatus.Read(daemonstatus.Path())
		if !ok || s.Op != daemonstatus.OpStopping || s.Phase != daemonstatus.PhaseAgents {
			return fmt.Errorf("status at engine shutdown = %+v (ok=%v)", s, ok)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-listening:
	case err := <-runDone:
		t.Fatalf("Run exited before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not reach the listener")
	}
	if atListen.Op != daemonstatus.OpStarting || atListen.Phase != daemonstatus.PhaseListening || atListen.PID != os.Getpid() {
		t.Fatalf("status before listening = %+v", atListen)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.mu.RLock()
		polled := len(d.sessions) > 0
		d.mu.RUnlock()
		if _, ok := daemonstatus.Read(daemonstatus.Path()); !ok && polled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status file outlived startup or the first poll never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run shutdown deadlocked")
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
	if len(eng.seen) != 1 || len(eng.seen[0].Agents) != 1 {
		t.Fatalf("agent progress = %+v", eng.seen)
	}
	if a := eng.seen[0].Agents[0]; a.ID != "agent-1" || a.Title != "fix login" || a.State != daemonstatus.AgentBusy || a.Until == 0 {
		t.Fatalf("published agent = %+v", a)
	}
	if _, err := os.Stat(daemonstatus.Path()); !os.IsNotExist(err) {
		t.Fatalf("status file left after shutdown: %v", err)
	}
}
