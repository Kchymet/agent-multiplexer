package sessionrpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openWatchedDir(t *testing.T) (string, *os.File) {
	t.Helper()
	path := t.TempDir()
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	return path, dir
}

func expectWake(t *testing.T, wake wakeSignal, want bool, what string) {
	t.Helper()
	wait := 100 * time.Millisecond
	if want {
		wait = 5 * time.Second
	}
	select {
	case <-wake:
		if !want {
			t.Fatalf("unexpected wake: %s", what)
		}
	case <-time.After(wait):
		if want {
			t.Fatalf("no wake: %s", what)
		}
	}
}

func TestWatchDirWakesOnPublication(t *testing.T) {
	path, dir := openWatchedDir(t)
	wake := newWakeSignal()
	watch, err := watchDir(dir, path, wake.notify)
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer watch.Close()

	// A publication temporary's own write is not a wake on Linux (kqueue
	// cannot tell names apart, so it may wake there); its rename into place is.
	temporary := filepath.Join(path, ".tmp-0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(temporary, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	drain(wake)
	if err := os.Rename(temporary, filepath.Join(path, "a.req")); err != nil {
		t.Fatal(err)
	}
	expectWake(t, wake, true, "rename into the directory")

	if err := os.WriteFile(filepath.Join(path, "b.req"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWake(t, wake, true, "direct write into the directory")

	watch.Close()
	drain(wake)
	if err := os.WriteFile(filepath.Join(path, "c.req"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWake(t, wake, false, "write after Close")
}

// Two registrations on one directory share the platform watch; closing one
// keeps the other.
func TestWatchDirSharedDirectory(t *testing.T) {
	path, dir := openWatchedDir(t)
	first, second := newWakeSignal(), newWakeSignal()
	a, err := watchDir(dir, path, first.notify)
	if err != nil {
		t.Fatal(err)
	}
	b, err := watchDir(dir, path, second.notify)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a.Close()
	drain(first)
	if err := os.WriteFile(filepath.Join(path, "a.req"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	expectWake(t, second, true, "remaining registration")
	expectWake(t, first, false, "closed registration")
}

// With both sides' polling pushed out of reach, a round trip can only finish
// through publication wakes: the server's on the request, the client's on the
// response.
func TestRoundTripIsEventDriven(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{PollInterval: time.Hour})
	if !f.server.Watched() {
		t.Skip("platform cannot watch the mailbox")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- f.server.Run(ctx) }()
	client := f.client(clientOptions{poll: time.Hour})
	for i := 0; i < 3; i++ {
		start := time.Now()
		callCtx, callCancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := client.Query(callCtx, Query{Verb: "version"})
		callCancel()
		if err != nil || result.Status != StatusOK {
			t.Fatalf("round trip %d = %+v, %v", i, result, err)
		}
		if elapsed := time.Since(start); elapsed >= WatchFallbackInterval {
			t.Fatalf("round trip %d took %v: served by a fallback, not a wake", i, elapsed)
		}
	}
	cancel()
	<-served
}

func drain(wake wakeSignal) {
	time.Sleep(50 * time.Millisecond)
	select {
	case <-wake:
	default:
	}
}
