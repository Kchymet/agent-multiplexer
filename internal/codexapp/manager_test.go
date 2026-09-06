package codexapp

import (
	"context"
	"testing"
)

// TestManagerEmptyLookups checks the no-supervisor paths: Get misses, Live is
// empty, and Close is a harmless no-op. These are the states the daemon hits on
// every steer/poll for a session that is not (or not yet) structured.
func TestManagerEmptyLookups(t *testing.T) {
	m := NewManager(context.Background(), "")
	if _, ok := m.Get("nope"); ok {
		t.Fatal("Get returned a supervisor for an unknown id")
	}
	if len(m.Live()) != 0 {
		t.Fatal("Live non-empty on a fresh manager")
	}
	m.Close("nope") // must not panic
	m.Shutdown()    // must not panic
	m.Close("nope") // idempotent after shutdown
}

// TestManagerForgetRemovesIdentity checks that Forget drops the persisted identity
// (session deleted for good), while Close alone leaves it (archive → resumable).
func TestManagerForgetRemovesIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := SaveIdentity(Identity{SessionID: "s1", ThreadID: "t1", ControlMode: "structured"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	m := NewManager(context.Background(), "")

	// Close on an unmanaged id leaves the identity in place.
	m.Close("s1")
	if _, ok := LoadIdentity("s1"); !ok {
		t.Fatal("Close removed the persisted identity; it should survive for resume")
	}

	// Forget removes it.
	m.Forget("s1")
	if _, ok := LoadIdentity("s1"); ok {
		t.Fatal("Forget left the persisted identity behind")
	}
}

// TestResumeThreadForGating checks the AGE-198 fix: a session is resumed ONLY once
// its thread has run a turn (Resumable), so a pinned-but-never-run thread is never
// resumed (which would 'no rollout found' and poison its first turn) — it starts
// fresh instead.
func TestResumeThreadForGating(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Current identity, not resumable (fresh thread, no turn) ⇒ do not resume.
	if err := SaveIdentity(Identity{SessionID: "g", ThreadID: "thr-1", Resumable: false, Version: identityVersion}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("g"); got != "" {
		t.Fatalf("resumeThreadFor(current, non-resumable) = %q, want empty", got)
	}

	// Resumable (ran a turn, or was previously resumed) ⇒ resume that thread.
	if err := SaveIdentity(Identity{SessionID: "g", ThreadID: "thr-1", Resumable: true, Version: identityVersion}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("g"); got != "thr-1" {
		t.Fatalf("resumeThreadFor(resumable) = %q, want thr-1", got)
	}

	// LEGACY identity (persisted before the Resumable field, Version 0) with a
	// thread id ⇒ still attempt resume — don't silently discard a real conversation
	// on upgrade (the handshake fallback covers a genuine miss).
	if err := SaveIdentity(Identity{SessionID: "legacy", ThreadID: "old-thr", Resumable: false, Version: 0}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("legacy"); got != "old-thr" {
		t.Fatalf("resumeThreadFor(legacy) = %q, want old-thr (must not discard a legacy conversation)", got)
	}

	// No identity ⇒ start fresh.
	if got := resumeThreadFor("unknown"); got != "" {
		t.Fatalf("resumeThreadFor(unknown) = %q, want empty", got)
	}
}
