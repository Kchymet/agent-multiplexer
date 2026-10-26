package daemon

import (
	"amux/internal/codexapp"
	"path/filepath"
	"testing"

	"amux/internal/buildinfo"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/store"
)

func TestQueryVersionReportsDaemonAndDatabaseContracts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	c, done := dialDaemon(t, testDaemon(t))
	defer done()

	got, err := c.Version()
	if err != nil {
		t.Fatal(err)
	}
	if got.DaemonVersion != buildinfo.Version || got.DaemonProtocol != buildinfo.DaemonProtocol {
		t.Errorf("daemon version = %+v, want %s protocol %d", got, buildinfo.Version, buildinfo.DaemonProtocol)
	}
	if got.DatabaseSchema != store.CurrentSchemaVersion ||
		got.DatabaseMinSchema != store.MinSchemaVersion ||
		got.DatabaseMaxSchema != store.CurrentSchemaVersion || got.DatabaseError != "" {
		t.Errorf("database version = %+v, want schema %d in supported range %d-%d",
			got, store.CurrentSchemaVersion, store.MinSchemaVersion, store.CurrentSchemaVersion)
	}
}

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
	if err := db.PutSession(store.Session{ID: "a1", RootID: "wg1", Agent: "claude", ClaudeID: cid, Dir: dir}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c, done := dialDaemon(t, testDaemon(t))
	defer done()

	got, err := c.RuntimePath("a1")
	if err != nil {
		t.Fatal(err)
	}
	// With no transcript on disk yet, the path is where the agent's PRIVATE Claude
	// home (not the user's) will write it.
	if want := claudecfg.At(claudecfg.AgentHome(dir)).TranscriptPath(dir, cid); got != want {
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

// TestQueryRuntimeRecordCarriesRuntime verifies the daemon answers the runtime
// record query with both the transcript path and the runtime that wrote it, so
// the provider picks the right reader and stamps the runtime on the frames it
// sends. A codex session resolves to its rollout; a session whose harness has no
// record resolves to a zero record.
func TestQueryRuntimeRecordCarriesRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	// Central Codex auth always runs Codex under the App Server, so both a
	// session pinned by the older PTY path and one never run resolve to the
	// structured event record (the App Server resumes the pinned rollout).
	if err := db.PutSession(store.Session{ID: "c1", RootID: "test-root", Agent: "codex", ClaudeID: "22222222-2222-4222-8222-222222222222", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "c2", RootID: "test-root", Agent: "codex", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c, done := dialDaemon(t, testDaemon(t))
	defer done()
	for _, id := range []string{"c1", "c2"} {
		got, err := c.RuntimeRecord(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Runtime != "codex" || !got.Structured || got.Path != codexapp.EventLogPathFor(id) {
			t.Errorf("RuntimeRecord(%s) = %+v, want the structured event record", id, got)
		}
		// The path-only query still answers for a provider that predates the
		// record query.
		if path, err := c.RuntimePath(id); err != nil || path != got.Path {
			t.Errorf("RuntimePath(%s) = %q, %v", id, path, err)
		}
	}
}
