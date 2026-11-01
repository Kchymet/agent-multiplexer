package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
)

func TestCoordinatorDirIsSeparateFromMemberAncestor(t *testing.T) {
	root := RootDir("wg")
	coordinator := CoordinatorDir("wg")
	member := AgentDir("wg", "agent")

	if coordinator != filepath.Join(root, "coordinator") {
		t.Fatalf("CoordinatorDir = %q, want child of %q", coordinator, root)
	}
	if coordinator == root || filepath.Dir(coordinator) != filepath.Dir(member) {
		t.Fatalf("coordinator %q and member %q must be distinct siblings", coordinator, member)
	}
}

// openTemp opens a store rooted at a fresh temp dir so a test never touches the
// user's DB.
func openTemp(t *testing.T) *DB {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	db, err := Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenVersionsTheSchema(t *testing.T) {
	db := openTemp(t)
	got, err := db.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if got != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, CurrentSchemaVersion)
	}
}

func TestOpenRejectsANewerSchema(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	future := CurrentSchemaVersion + 1
	if _, err := raw.Exec(`PRAGMA user_version = ` + fmt.Sprint(future)); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if db != nil {
		db.Close()
		t.Fatal("Open returned a database for an unsupported future schema")
	}
	var versionErr *SchemaVersionError
	if !errors.As(err, &versionErr) || versionErr.Have != future ||
		versionErr.Min != MinSchemaVersion || versionErr.Max != CurrentSchemaVersion {
		t.Fatalf("Open error = %v, want schema %d outside range %d-%d",
			err, future, MinSchemaVersion, CurrentSchemaVersion)
	}
}

// TestFieldScopedUpdatersTouchOneColumn pins the core contract behind AGE-133:
// each updater writes only its own field and leaves every other column of the row
// intact — which is what stops one writer from reverting another's change.
func TestFieldScopedUpdatersTouchOneColumn(t *testing.T) {
	db := openTemp(t)

	seed := Session{
		ID: "a1", RootID: "r1", Name: "orig", Agent: "claude", Model: "opus",
		Mode: ModeTask, Repo: "acme/api", Branch: "amux/r1-a1", Dir: "/d",
		ClaudeID: "cid-orig", Prompt: "p", Created: 100, Scope: ScopeWork,
	}
	if err := db.PutSession(seed); err != nil {
		t.Fatal(err)
	}

	// Each updater changes exactly its field; a full re-read confirms nothing else moved.
	if err := db.SetName("a1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetModel("a1", "sonnet"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetClaudeID("a1", "cid-new"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRepoScope("a1", "acme/web"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRootID("a1", "r2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag("a1", true, 555); err != nil {
		t.Fatal(err)
	}

	got, ok, err := db.GetSession("a1")
	if err != nil || !ok {
		t.Fatalf("GetSession: ok=%v err=%v", ok, err)
	}
	want := Session{
		ID: "a1", RootID: "r2", Name: "renamed", Agent: "claude", Model: "sonnet",
		Mode: ModeTask, Repo: "acme/web", Branch: "amux/r1-a1", Dir: "/d",
		ClaudeID: "cid-new", Prompt: "p", Created: 100, Scope: ScopeWork,
		Archived: true, ArchivedAt: 555,
	}
	if got != want {
		t.Errorf("after field-scoped updates:\n got  %+v\n want %+v", got, want)
	}
}

// TestFieldScopedUpdaterMissingIDNoError verifies an updater on an absent id is a
// harmless no-op — callers that need a not-found error check existence first.
func TestFieldScopedUpdaterMissingIDNoError(t *testing.T) {
	db := openTemp(t)
	if err := db.SetName("nope", "x"); err != nil {
		t.Errorf("SetName on missing id = %v, want nil (no-op)", err)
	}
	if _, ok, _ := db.GetSession("nope"); ok {
		t.Error("SetName on a missing id should not create a row")
	}
}

// SessionHeads is the daemon's per-tick reconcile scan: it must agree with
// AllSessions on the fields it returns and be answered from the covering index
// alone, never the rows (and their prompts' overflow pages).
func TestSessionHeadsMatchAllSessionsFromACoveringIndex(t *testing.T) {
	db := openTemp(t)
	for i, s := range []Session{
		{ID: "root", Name: "r", Created: 1, Prompt: "x"},
		{ID: "b", RootID: "root", Created: 3, Archived: true, Prompt: string(make([]byte, 16<<10))},
		{ID: "a", RootID: "root", Created: 2},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatalf("PutSession %d: %v", i, err)
		}
	}
	all, err := db.AllSessions()
	if err != nil {
		t.Fatal(err)
	}
	heads, err := db.SessionHeads()
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != len(all) {
		t.Fatalf("SessionHeads = %d rows, AllSessions = %d", len(heads), len(all))
	}
	for i := range all {
		want := Session{ID: all[i].ID, RootID: all[i].RootID, Archived: all[i].Archived}
		if heads[i] != want {
			t.Fatalf("SessionHeads[%d] = %+v, want %+v", i, heads[i], want)
		}
	}
	rows, err := db.sql.Query(`EXPLAIN QUERY PLAN SELECT id,root_id,archived FROM sessions ORDER BY created`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if want := "USING COVERING INDEX idx_sessions_heads"; !strings.Contains(plan, want) {
		t.Fatalf("query plan %q does not contain %q", plan, want)
	}
}
