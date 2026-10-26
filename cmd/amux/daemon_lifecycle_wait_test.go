package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine/local"
)

func stubStartupProbe(t *testing.T, probe func() error) {
	t.Helper()
	old := startupProbe
	startupProbe = probe
	t.Cleanup(func() { startupProbe = old })
}

// The old daemon's engine waits for busy agents to reach a safe point before
// SIGTERM/SIGKILL. A shorter client wait reported every restart issued while
// any agent worked as a failed shutdown and never started the replacement.
func TestDaemonStopWaitCoversEngineShutdownBudget(t *testing.T) {
	t.Setenv("AMUX_SHUTDOWN_GRACE", "")
	t.Setenv("AMUX_KILL_GRACE", "")
	if got, engine := daemonStopBudget(), local.ShutdownBudget(); got <= engine {
		t.Fatalf("stop wait %v does not outlast engine shutdown budget %v", got, engine)
	}
	t.Setenv("AMUX_SHUTDOWN_GRACE", "2m")
	if got := daemonStopBudget(); got <= 2*time.Minute {
		t.Fatalf("stop wait %v ignores a configured shutdown grace", got)
	}
}

func TestDaemonStopWaitsForSlowSingletonRelease(t *testing.T) {
	sandboxCLI(t)
	if err := os.MkdirAll(core.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(core.DaemonLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	fd := int(lock.Fd())
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(600 * time.Millisecond)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
	}()
	defer func() { <-released }()
	stubStartupProbe(t, func() error { return syscall.ECONNREFUSED })
	old := daemonStopBudget
	daemonStopBudget = func() time.Duration { return 5 * time.Second }
	t.Cleanup(func() { daemonStopBudget = old })

	// daemonStop's shutdown request needs a live daemon; exercise the wait it
	// returns through directly.
	start := time.Now()
	if err := waitDaemonStopped(); err != nil {
		t.Fatalf("stop wait failed while teardown was still within budget: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("stop reported success before the singleton was released (%v)", elapsed)
	}
}

// A daemon that is alive but still reconciling its mailboxes must be waited
// for: a fixed 3s window reported failure although it came up moments later.
func TestDaemonStartWaitsForSlowLiveDaemon(t *testing.T) {
	ready := time.Now().Add(3500 * time.Millisecond)
	stubStartupProbe(t, func() error {
		if time.Now().Before(ready) {
			return syscall.ENOENT
		}
		return nil
	})
	exited := make(chan error) // never: the child is still running
	if err := awaitDaemonReady(syscall.ENOENT, exited, 30*time.Second, nil); err != nil {
		t.Fatalf("slow but live daemon reported as failed: %v", err)
	}
}

func TestDaemonStartReportsExitedChildPromptly(t *testing.T) {
	sandboxCLI(t)
	stubStartupProbe(t, func() error { return syscall.ECONNREFUSED })
	exited := make(chan error, 1)
	exited <- errors.New("exit status 1")
	start := time.Now()
	err := awaitDaemonReady(syscall.ENOENT, exited, 30*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "stopped before accepting") || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("exited child error = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("exited child waited out the startup budget")
	}
}

func TestDaemonStartAcceptsOtherOwnerAfterChildExits(t *testing.T) {
	calls := 0
	stubStartupProbe(t, func() error {
		calls++
		if calls == 1 {
			return syscall.ECONNREFUSED
		}
		return nil // another daemon won the singleton and answers
	})
	exited := make(chan error, 1)
	exited <- nil
	if err := awaitDaemonReady(syscall.ENOENT, exited, 30*time.Second, nil); err != nil {
		t.Fatalf("answering daemon after a clean child exit reported as failure: %v", err)
	}
}
