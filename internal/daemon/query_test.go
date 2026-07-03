package daemon

import (
	"path/filepath"
	"testing"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/store"
)

// TestQuerySnapshotServesCachedRail verifies the daemon serves its already-computed
// session rail over ActionQuery, so a peer (the provider) publishes the inventory
// without opening the store itself.
func TestQuerySnapshotServesCachedRail(t *testing.T) {
	d := testDaemon(t)
	// The poll loop normally fills this; set it directly for the test.
	d.sessions = []core.Session{{ID: "a1"}, {ID: "a2"}}

	c, done := dialDaemon(t, d)
	defer done()

	got, err := c.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a2" {
		t.Fatalf("Snapshot() = %+v, want the two cached sessions", got)
	}
}

// TestQueryRuntimePathResolvesViaHarness verifies the daemon resolves a tracked
// session id to its harness transcript path (so the provider tails transcripts via
// the daemon), and returns "" for an unknown id.
func TestQueryRuntimePathResolvesViaHarness(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate claudecfg.ListSessions fallback
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	const cid = "11111111-1111-4111-8111-111111111111"
	dir := t.TempDir()
	if err := db.PutSession(store.Session{ID: "a1", Agent: "claude", ClaudeID: cid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c, done := dialDaemon(t, testDaemon(t))
	defer done()

	got, err := c.RuntimePath("a1")
	if err != nil {
		t.Fatal(err)
	}
	if want := claudecfg.TranscriptPath(dir, cid); got != want {
		t.Errorf("RuntimePath(a1) = %q, want %q", got, want)
	}

	// An unknown id (no store row, no untracked Claude session) resolves to "".
	missing, err := c.RuntimePath("nope")
	if err != nil {
		t.Fatal(err)
	}
	if missing != "" {
		t.Errorf("RuntimePath(nope) = %q, want empty", missing)
	}
}
