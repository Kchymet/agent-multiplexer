package codexapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Concurrent starts (creation and native attach) submit once; a daemon restart
// resumes the persisted identity without replaying the creation prompt.
func TestManagerInitialPromptOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	servers := make(chan *fakeServer, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		servers <- fs
		fs.loop()
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewManager(ctx, "")
	defer m.Shutdown()
	// The in-process fake owns the socket; this child exercises process lifetime
	// without invoking a real model. Shutdown kills it immediately.
	ensure := func(m *Manager) (*Supervisor, error) {
		return m.Ensure("initial", "", nil, []string{"sleep", "60"}, endpoint, "fix it")
	}
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() { _, err := ensure(m); errs <- err }()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	fs := <-servers
	fs.mu.Lock()
	turns := 0
	for _, call := range fs.calls {
		if call.Method == "turn/start" {
			turns++
		}
	}
	fs.mu.Unlock()
	if turns != 1 {
		t.Fatalf("concurrent starts submitted %d prompts, want 1", turns)
	}
	id, ok := LoadIdentity("initial")
	if !ok || !id.Resumable {
		t.Fatalf("missing resumable identity: %+v", id)
	}
	m.Shutdown()
	restarted := NewManager(ctx, "")
	defer restarted.Shutdown()
	sup, err := ensure(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if sup.ThreadID() != id.ThreadID {
		t.Fatal("restart changed the thread")
	}
	resumed := <-servers
	if _, ok := resumed.sawCall("turn/start"); ok {
		t.Fatal("restart replayed the initial prompt")
	}
	if _, ok := resumed.sawCall("thread/start"); ok {
		t.Fatal("restart created a fresh thread")
	}
}

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
