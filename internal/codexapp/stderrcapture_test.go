package codexapp

import (
	"context"
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
