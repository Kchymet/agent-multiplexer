package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
)

func identityOf(t *testing.T, db *DB, id string) (SessionIdentity, bool) {
	t.Helper()
	m, err := db.SessionIdentities([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := m[id]
	return s, ok
}

func rawIncarnation(t *testing.T, db *DB, id string) string {
	t.Helper()
	var inc string
	if err := db.sql.QueryRow(`SELECT incarnation FROM sessions WHERE id=?`, id).Scan(&inc); err != nil {
		t.Fatal(err)
	}
	return inc
}

// CreateSession returns a complete identity; the incarnation is what the row
// holds, 128 random bits, and a new workgroup's root identity is its own.
func TestCreateSessionIdentity(t *testing.T) {
	db := openTemp(t)
	root, err := db.CreateSession(Session{ID: "wg", Scope: ScopeWork}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !root.Complete() || root.RootID != "wg" || root.RootIncarnation != root.Incarnation || len(root.Incarnation) != 32 {
		t.Fatalf("root identity = %+v", root)
	}
	if got := rawIncarnation(t, db, "wg"); got != root.Incarnation {
		t.Fatalf("stored incarnation %q != returned %q", got, root.Incarnation)
	}
	member, err := db.CreateSession(Session{ID: "a1", RootID: "wg"}, root.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if member.RootID != "wg" || member.RootIncarnation != root.Incarnation || member.Incarnation == root.Incarnation {
		t.Fatalf("member identity = %+v", member)
	}
	// Plain INSERT: never overwrites, and the existing row is untouched.
	if _, err := db.CreateSession(Session{ID: "a1", RootID: "wg", Name: "dup"}, ""); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate create = %v, want ErrSessionExists", err)
	}
	if s, _, _ := db.GetSession("a1"); s.Name != "" || rawIncarnation(t, db, "a1") != member.Incarnation {
		t.Fatalf("duplicate create changed the row: %+v", s)
	}
	// A root takes no expected root.
	if _, err := db.CreateSession(Session{ID: "wg2"}, "x"); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("root with expectRoot = %v", err)
	}
}

// A member is refused when its root is missing, not a root, or replaced, and
// nothing is written.
func TestCreateSessionRefusesChangedRoot(t *testing.T) {
	db := openTemp(t)
	root, _ := db.CreateSession(Session{ID: "wg"}, "")
	if _, err := db.CreateSession(Session{ID: "m", RootID: "wg"}, root.Incarnation); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, root, expect string
	}{
		{"missing root", "nope", ""},
		{"member as root", "m", ""},
		{"forged expectation", "wg", "00000000000000000000000000000000"},
	}
	for _, c := range cases {
		if _, err := db.CreateSession(Session{ID: "x", RootID: c.root}, c.expect); !errors.Is(err, ErrRootChanged) {
			t.Fatalf("%s: %v, want ErrRootChanged", c.name, err)
		}
		if _, ok := identityOf(t, db, "x"); ok {
			t.Fatalf("%s: a refused member was written", c.name)
		}
	}
	// Delete and recreate the root under the same id: the old incarnation no
	// longer admits a member.
	if err := db.DeleteSession("wg"); err != nil {
		t.Fatal(err)
	}
	again, err := db.CreateSession(Session{ID: "wg"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if again.Incarnation == root.Incarnation {
		t.Fatal("a reused id got the same incarnation")
	}
	if _, err := db.CreateSession(Session{ID: "y", RootID: "wg"}, root.Incarnation); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("member under replaced root = %v", err)
	}
}

// The atomic check: a root replaced after the read-only precheck (and after
// the account binding) and before the insert transaction is still refused,
// and nothing is written.
func TestCreateSessionAtomicRootCheck(t *testing.T) {
	db := openTemp(t)
	root, _ := db.CreateSession(Session{ID: "wg"}, "")
	for _, replace := range []string{
		`UPDATE sessions SET incarnation='replaced' WHERE id='wg'`,
		`DELETE FROM sessions WHERE id='wg'`,
		`UPDATE sessions SET root_id='other' WHERE id='wg'`,
	} {
		db.sql.Exec(`DELETE FROM sessions`)
		db.sql.Exec(`INSERT INTO sessions(id,root_id,incarnation) VALUES('wg','',?)`, root.Incarnation)
		afterCreatePrecheck = func(Session) {
			if _, err := db.sql.Exec(replace); err != nil {
				t.Error(err)
			}
		}
		_, err := db.CreateSession(Session{ID: "m", RootID: "wg"}, root.Incarnation)
		afterCreatePrecheck = func(Session) {}
		if !errors.Is(err, ErrRootChanged) {
			t.Fatalf("%s: %v, want ErrRootChanged", replace, err)
		}
		if _, ok := identityOf(t, db, "m"); ok {
			t.Fatalf("%s: member written", replace)
		}
	}
	// And an id taken in the same window is ErrSessionExists, row untouched.
	afterCreatePrecheck = func(Session) {
		db.sql.Exec(`INSERT INTO sessions(id,root_id,name,incarnation) VALUES('z','','other','keep')`)
	}
	defer func() { afterCreatePrecheck = func(Session) {} }()
	if _, err := db.CreateSession(Session{ID: "z"}, ""); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("racing id = %v", err)
	}
	if rawIncarnation(t, db, "z") != "keep" {
		t.Fatal("racing row overwritten")
	}
}

// PutSession's upsert and every field setter preserve the incarnation; a new
// row written by PutSession gets one. The Session type has no incarnation, so
// a caller cannot supply one.
func TestUpsertAndSettersPreserveIncarnation(t *testing.T) {
	db := openTemp(t)
	ident, _ := db.CreateSession(Session{ID: "wg", Scope: ScopeWork}, "")
	m, _ := db.CreateSession(Session{ID: "a", RootID: "wg"}, ident.Incarnation)
	if err := db.PutSession(Session{ID: "a", RootID: "wg", Name: "renamed", Archived: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []func() error{
		func() error { return db.SetName("a", "n") },
		func() error { return db.SetModel("a", "m") },
		func() error { return db.SetClaudeID("a", "c") },
		func() error { return db.SetArchivedFlag("a", false, 0) },
		func() error { return db.SetDir("a", "/d") },
		func() error { return db.SetRepoScope("a", "r") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	if got := rawIncarnation(t, db, "a"); got != m.Incarnation {
		t.Fatalf("incarnation changed by upsert/setters: %q -> %q", m.Incarnation, got)
	}
	// SetRootID moves a member without changing its token: identity checks
	// must compare the root id too (SessionIdentity reports it).
	if _, err := db.CreateSession(Session{ID: "wg2"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRootID("a", "wg2"); err != nil {
		t.Fatal(err)
	}
	cur, _ := identityOf(t, db, "a")
	if cur.Incarnation != m.Incarnation || cur.RootID != "wg2" {
		t.Fatalf("after move: %+v", cur)
	}
	if err := db.PutSession(Session{ID: "fresh", RootID: "wg"}); err != nil {
		t.Fatal(err)
	}
	if inc := rawIncarnation(t, db, "fresh"); len(inc) != 32 {
		t.Fatalf("PutSession insert incarnation = %q", inc)
	}
}

// A database migrated before this feature (and every other column) gains
// incarnations for every row atomically with the migration.
func TestMigrationBackfillsIncarnations(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	seedLegacySchema(t)
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if inc := rawIncarnation(t, db, "wg"); len(inc) != 32 {
		t.Fatalf("legacy root incarnation = %q", inc)
	}
	var empty int
	if err := db.sql.QueryRow(`SELECT count(*) FROM sessions WHERE incarnation=''`).Scan(&empty); err != nil || empty != 0 {
		t.Fatalf("rows without incarnation = %d, %v", empty, err)
	}
}

// An older binary's INSERT (which never names the column) after the
// migration leaves a row with the ” default. It matches nothing, and the
// next open — the fast path included — backfills it; existing incarnations
// are untouched.
func TestOldBinaryInsertIsBackfilledOnReopen(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-handles", true: "shared-handles"}[shared], func(t *testing.T) {
			ShareHandles(shared)
			t.Cleanup(func() { ShareHandles(false) })
			db := openTemp(t)
			root, _ := db.CreateSession(Session{ID: "wg"}, "")
			raw, err := sql.Open("sqlite", core.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			// The exact column list an older binary's putSessionRow inserts.
			if _, err := raw.Exec(`INSERT INTO sessions(id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created,scope,archived,archived_at)
				VALUES('old','wg','','claude','','task','','','','','',1,'work',0,0)`); err != nil {
				t.Fatal(err)
			}
			if cur, ok := identityOf(t, db, "old"); !ok || cur.Incarnation != "" {
				t.Fatalf("old-binary row before reopen = %+v %v", cur, ok)
			}
			db2, err := Open() // process fast path: already migrated
			if err != nil {
				t.Fatal(err)
			}
			defer db2.Close()
			cur, _ := identityOf(t, db2, "old")
			if len(cur.Incarnation) != 32 {
				t.Fatalf("old-binary row after reopen = %+v", cur)
			}
			if got := rawIncarnation(t, db2, "wg"); got != root.Incarnation {
				t.Fatalf("backfill changed an existing incarnation")
			}
		})
	}
}

// A root an older binary inserted without an incarnation gets one inside the
// member creation's transaction, so the identity is complete.
func TestCreateMemberUnderOldBinaryRoot(t *testing.T) {
	db := openTemp(t)
	if _, err := db.sql.Exec(`INSERT INTO sessions(id,root_id) VALUES('legacy','')`); err != nil {
		t.Fatal(err)
	}
	m, err := db.CreateSession(Session{ID: "m", RootID: "legacy"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Complete() || rawIncarnation(t, db, "legacy") != m.RootIncarnation {
		t.Fatalf("identity = %+v", m)
	}
}

// Replacing the store (a new database file) gives reused ids new identities.
func TestStoreReplacementGivesNewIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "a"))
	db, _ := Open()
	first, _ := db.CreateSession(Session{ID: "wg"}, "")
	db.Close()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "b"))
	db2, _ := Open()
	defer db2.Close()
	second, _ := db2.CreateSession(Session{ID: "wg"}, "")
	if first.Incarnation == second.Incarnation {
		t.Fatal("replacement store reused an incarnation")
	}
}

// Concurrent first opens of a pre-feature store all succeed and leave every
// row with exactly one incarnation (the backfill commits with the migration).
func TestConcurrentOpensBackfillOnce(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	seedLegacySchema(t)
	for i, err := range openConcurrently(concurrentOpeners) {
		if err != nil {
			t.Fatalf("opener %d: %v", i, err)
		}
	}
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	first := rawIncarnation(t, db, "wg")
	db2, _ := Open()
	defer db2.Close()
	if len(first) != 32 || rawIncarnation(t, db2, "wg") != first {
		t.Fatal("incarnation not stable across opens")
	}
}

// The column, index and backfill commit only with the rest of the migration:
// a migration transaction that fails after them leaves none of them.
func TestIncarnationMigrationRollsBack(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	seedLegacySchema(t)
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	d := &DB{sql: raw}
	boom := errors.New("later migration step failed")
	if err := d.writeTx(func(q querier) error {
		if err := migrateIncarnations(q); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("writeTx = %v", err)
	}
	var cols int
	if err := raw.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name='incarnation'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("incarnation column survived a rolled-back migration: %d %v", cols, err)
	}
}

func TestSessionIdentitiesBound(t *testing.T) {
	db := openTemp(t)
	ids := make([]string, MaxIdentityLookup+1)
	for i := range ids {
		ids[i] = "x"
	}
	if _, err := db.SessionIdentities(ids); err == nil {
		t.Fatal("unbounded lookup accepted")
	}
	if m, err := db.SessionIdentities(ids[:MaxIdentityLookup]); err != nil || len(m) != 0 {
		t.Fatalf("bounded lookup = %v %v", m, err)
	}
}

// writePreIncarnationV3 writes, at path, a valid schema-3 store as a
// pre-incarnation binary left it: every current table and column except
// sessions.incarnation, holding one root row.
func writePreIncarnationV3(t *testing.T, path string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "donor"))
	donor, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	donor.PutSession(Session{ID: "restored", Scope: ScopeWork})
	donor.Close()
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX idx_sessions_no_incarnation`,
		`ALTER TABLE sessions DROP COLUMN incarnation`,
		`PRAGMA journal_mode=DELETE`,
		`VACUUM INTO '` + path + `'`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()
}

// Restoring a pre-incarnation schema-3 file over a path this process already
// migrated (so Open takes its cached fast path, and the version still matches)
// must still add and backfill the column, not fail on it.
func TestRestoredPreIncarnationStoreOnCachedPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "data")
	t.Setenv("XDG_DATA_HOME", home)
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	first, _ := db.CreateSession(Session{ID: "wg"}, "")
	db.Close()
	path := core.DBPath()

	restored := filepath.Join(t.TempDir(), "restored.db")
	writePreIncarnationV3(t, restored)
	t.Setenv("XDG_DATA_HOME", home)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
	b, err := os.ReadFile(restored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, cached := migratedStores.Load(path); !cached {
		t.Fatal("precondition: path should be cached as migrated")
	}
	db2, err := Open()
	if err != nil {
		t.Fatalf("Open of a restored pre-incarnation store on a cached path: %v", err)
	}
	defer db2.Close()
	cur, ok := identityOf(t, db2, "restored")
	if !ok || len(cur.Incarnation) != 32 {
		t.Fatalf("restored row identity = %+v %v", cur, ok)
	}
	if _, ok := identityOf(t, db2, "wg"); ok {
		t.Fatal("restored store still has the replaced row")
	}
	if again, err := db2.CreateSession(Session{ID: "wg"}, ""); err != nil || again.Incarnation == first.Incarnation {
		t.Fatalf("reused id after restore = %+v %v", again, err)
	}
}
