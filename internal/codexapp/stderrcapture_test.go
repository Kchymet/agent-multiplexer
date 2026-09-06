package codexapp

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStderrRingKeepsBoundedTail(t *testing.T) {
	r := newStderrRing(16)
	var full strings.Builder
	for i := 0; i < 100; i++ { // 1000 bytes through a 16-byte ring
		const s = "0123456789"
		full.WriteString(s)
		if _, err := r.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	want := full.String()[full.Len()-16:]
	if got := r.tail(); got != want {
		t.Fatalf("ring tail = %q, want last-16 %q", got, want)
	}

	// A single oversized write keeps only its last max bytes (never the whole write).
	big := make([]byte, 100)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	r2 := newStderrRing(16)
	if _, err := r2.Write(big); err != nil {
		t.Fatal(err)
	}
	if got := r2.tail(); len(got) != 16 || got != string(big[len(big)-16:]) {
		t.Fatalf("oversized write tail = %q (len %d)", got, len(got))
	}
}

// TestStartSurfacesChildStderrOnDialTimeout verifies the diagnostic: a child that
// writes a distinct line to stderr then exits WITHOUT ever binding the listener (the
// shape of a sandbox execvp ENOENT — bwrap prints to stderr and exits) must have that
// line surfaced in the error Start returns, not be swallowed behind a bare dial
// timeout.
func TestStartSurfacesChildStderrOnDialTimeout(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "cx.sock")
	sup := New(Config{
		SessionID:   "stderr-diag",
		Endpoint:    "unix://" + sock,
		DialTimeout: 200 * time.Millisecond, // never listens; fail fast
	})
	const marker = "AMUX_STDERR_MARKER_execvp_ENOENT"
	argv := []string{"sh", "-c", "echo " + marker + " 1>&2; exit 127"}

	err := sup.Start(context.Background(), argv)
	if err == nil {
		_ = sup.Close()
		t.Fatal("Start must fail when the child never listens")
	}
	if !strings.Contains(err.Error(), marker) {
		t.Fatalf("Start error omitted the child stderr tail: %v", err)
	}
}

// TestStartBoundedWhenDescendantHoldsStderr is the AGE-198 lifecycle regression: a
// failing child that leaves a DETACHED descendant holding the stderr pipe open must
// not wedge Start. os/exec's copier goroutine blocks on that pipe's EOF, and killProc
// waits for the copier via cmd.Wait — so without a bound Start hangs until the holder
// exits (AGE-198 measured 6s+, unbounded). cmd.WaitDelay must force the pipe closed
// and let Start return promptly, still carrying the child's own stderr tail.
func TestStartBoundedWhenDescendantHoldsStderr(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available to detach a stderr-holding descendant")
	}
	sock := filepath.Join(t.TempDir(), "cx.sock")
	sup := New(Config{
		SessionID:   "stderr-held",
		Endpoint:    "unix://" + sock,
		DialTimeout: 200 * time.Millisecond,
	})
	const marker = "AMUX_HELD_STDERR_MARKER"
	// Write a distinct stderr line, then start a session-detached (setsid) sleep that
	// inherits and keeps the stderr pipe open far longer than the test, and exit before
	// listening. The detached sleep escapes the process-group kill, so only WaitDelay
	// bounds the wait.
	argv := []string{"sh", "-c", "echo " + marker + " 1>&2; setsid sleep 30 & exit 127"}

	done := make(chan error, 1)
	go func() { done <- sup.Start(context.Background(), argv) }()
	select {
	case err := <-done:
		if err == nil {
			_ = sup.Close()
			t.Fatal("Start must fail when the child never listens")
		}
		if !strings.Contains(err.Error(), marker) {
			t.Fatalf("Start error omitted the child stderr tail: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start hung on a descendant holding the stderr pipe — cmd.WaitDelay is not bounding cmd.Wait")
	}
}
