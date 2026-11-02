package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"amux/internal/core"
)

const concurrentOpeners = 8

// seedLegacySchema writes a pre-versioning (user_version 0) database that lacks
// every column and table added since, so Open must run the whole upgrade.
func seedLegacySchema(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`
CREATE TABLE repos (name TEXT PRIMARY KEY, source TEXT NOT NULL, git_dir TEXT NOT NULL);
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, root_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT 'claude', model TEXT NOT NULL DEFAULT '',
  mode TEXT NOT NULL DEFAULT 'task', repo TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '', dir TEXT NOT NULL DEFAULT '',
  claude_id TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL DEFAULT 0);
CREATE TABLE coordinator_grant_state (coordinator_id TEXT PRIMARY KEY, initialized INTEGER NOT NULL CHECK(initialized = 1));
INSERT INTO repos VALUES('acme','/src/acme','/git/acme');
INSERT INTO sessions(id,root_id,repo,created) VALUES('wg','', 'acme', 1);
`); err != nil {
		t.Fatal(err)
	}
}

// openConcurrently races n first Opens of the current XDG data dir and returns
// every error.
func openConcurrently(n int) []error {
	start := make(chan struct{})
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := Open()
			if err == nil {
				err = db.Close()
			}
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func checkMigrated(t *testing.T, label string) {
	t.Helper()
	db, err := OpenReadOnly()
	if err != nil {
		t.Fatalf("%s: OpenReadOnly after concurrent Open: %v", label, err)
	}
	defer db.Close()
	if v, err := db.SchemaVersion(); err != nil || v != CurrentSchemaVersion {
		t.Fatalf("%s: schema version = %d, %v; want %d", label, v, err, CurrentSchemaVersion)
	}
	var markers int
	if err := db.sql.QueryRow(`SELECT count(*) FROM coordinator_grant_migrations`).Scan(&markers); err != nil || markers != 1 {
		t.Fatalf("%s: grant migration markers = %d, %v; want exactly 1", label, markers, err)
	}
}

// Concurrent first Opens in one process (the daemon starts its poll and socket
// loops together) must all succeed and migrate exactly once, for a fresh and an
// upgraded database, with and without shared handles.
func TestConcurrentFirstOpenMigratesOnce(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, upgrade := range []bool{false, true} {
			name := fmt.Sprintf("shared=%v/upgrade=%v", shared, upgrade)
			t.Run(name, func(t *testing.T) {
				ShareHandles(shared)
				t.Cleanup(func() { ShareHandles(false) })
				for round := range 10 {
					t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
					if upgrade {
						seedLegacySchema(t)
					}
					for i, err := range openConcurrently(concurrentOpeners) {
						if err != nil {
							t.Fatalf("round %d opener %d: %v", round, i, err)
						}
					}
					checkMigrated(t, fmt.Sprintf("round %d", round))
				}
			})
		}
	}
}

const helperOpenEnv = "AMUX_STORE_TEST_HELPER_OPEN"

// TestHelperProcessOpen is not a test: it is re-executed as a child process by
// TestConcurrentFirstOpenAcrossProcesses to perform one Open.
func TestHelperProcessOpen(t *testing.T) {
	if os.Getenv(helperOpenEnv) != "1" {
		t.Skip("helper process only")
	}
	db, err := Open()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "close:", err)
		os.Exit(1)
	}
}

// The daemon and CLI are separate processes that can both perform a first Open
// (e.g. a CLI command racing a freshly upgraded daemon), so the migration must
// also be serialized in the database, not only by an in-process lock.
func TestConcurrentFirstOpenAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	for _, seed := range []string{"fresh", "upgrade", "registry"} {
		t.Run(seed, func(t *testing.T) {
			for round := range 3 {
				dataHome := filepath.Join(t.TempDir(), "data")
				t.Setenv("XDG_DATA_HOME", dataHome)
				switch seed {
				case "upgrade":
					seedLegacySchema(t)
				case "registry":
					seedLegacyRegistry(t)
				}
				cmds := make([]*exec.Cmd, concurrentOpeners)
				outs := make([][]byte, concurrentOpeners)
				errs := make([]error, concurrentOpeners)
				var wg sync.WaitGroup
				for i := range cmds {
					cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessOpen$", "-test.count=1")
					cmd.Env = append(os.Environ(), helperOpenEnv+"=1", "XDG_DATA_HOME="+dataHome)
					cmds[i] = cmd
				}
				for i, cmd := range cmds {
					wg.Add(1)
					go func() {
						defer wg.Done()
						outs[i], errs[i] = cmd.CombinedOutput()
					}()
				}
				wg.Wait()
				for i, err := range errs {
					if err != nil {
						t.Fatalf("round %d process %d: %v\n%s", round, i, err, outs[i])
					}
				}
				checkMigrated(t, fmt.Sprintf("round %d", round))
				if seed == "registry" {
					checkRegistryImportedOnce(t, fmt.Sprintf("round %d", round))
				}
			}
		})
	}
}

func seedLegacyRegistry(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := `{"workspaces":[{"id":"ws","agent":"claude","repos":["acme"],"dir":"/d","created":1}]}`
	if err := os.WriteFile(core.RegistryPath(), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkRegistryImportedOnce(t *testing.T, label string) {
	t.Helper()
	db, err := OpenReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	subs, err := db.Children("ws")
	db.Close()
	if err != nil || len(subs) != 1 {
		t.Fatalf("%s: legacy workspace has %d sub-sessions (%v), want 1", label, len(subs), err)
	}
	if _, err := os.Stat(core.RegistryPath() + ".migrated"); err != nil {
		t.Fatalf("%s: registry backup: %v", label, err)
	}
}

// Racing first opens must import the legacy JSON registry once: each import
// adds a sub-session under a fresh id, so a double import duplicates it.
func TestConcurrentFirstOpenImportsLegacyRegistryOnce(t *testing.T) {
	for round := range 5 {
		t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
		seedLegacyRegistry(t)
		for i, err := range openConcurrently(concurrentOpeners) {
			if err != nil {
				t.Fatalf("round %d opener %d: %v", round, i, err)
			}
		}
		checkRegistryImportedOnce(t, fmt.Sprintf("round %d", round))
	}
}

// A migration that fails part-way must leave the database as it was (no
// half-applied DDL or advanced version), record nothing process-wide, and be
// retried in full by the next Open once the obstacle is gone.
func TestFailedMigrationRollsBackAndRetries(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	seedLegacySchema(t)
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// all_repos is added last; occupying it with an incompatible view name makes
	// that ALTER fail after every earlier step succeeded.
	if _, err := raw.Exec(`DROP TABLE coordinator_grant_state; CREATE VIEW coordinator_grant_state AS SELECT 1 AS coordinator_id`); err != nil {
		t.Fatal(err)
	}
	for i, err := range openConcurrently(concurrentOpeners) {
		if err == nil {
			t.Fatalf("opener %d: Open succeeded over a blocking view", i)
		}
	}
	var version, scope int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 0 {
		t.Fatalf("user_version after failed migration = %d, %v; want 0", version, err)
	}
	if err := raw.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name='scope'`).Scan(&scope); err != nil || scope != 0 {
		t.Fatalf("sessions.scope present after failed migration (%d, %v); want rolled back", scope, err)
	}
	if _, done := migratedStores.Load(core.DBPath()); done {
		t.Fatal("failed migration was recorded as complete")
	}
	migrationLocksMu.Lock()
	leaked := len(migrationLocks)
	migrationLocksMu.Unlock()
	if leaked != 0 {
		t.Fatalf("%d migration locks left after Open returned", leaked)
	}

	if _, err := raw.Exec(`DROP VIEW coordinator_grant_state`); err != nil {
		t.Fatal(err)
	}
	for i, err := range openConcurrently(concurrentOpeners) {
		if err != nil {
			t.Fatalf("retry opener %d: %v", i, err)
		}
	}
	checkMigrated(t, "retry")
}

const twoWorkspaceRegistry = `{"repos":[{"name":"acme","source":"/src/acme","gitDir":"/git/acme"}],
"workspaces":[{"id":"ws","agent":"claude","repos":["acme"],"dir":"/d","created":1},
{"id":"boom","agent":"codex","repos":["acme"],"dir":"/e","created":2}]}`

// seedEmptyStoreWithRegistry writes a legacy schema with no sessions plus a
// registry to import, and returns a raw handle for arranging failures.
func seedEmptyStoreWithRegistry(t *testing.T, registry string) *sql.DB {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	seedLegacySchema(t)
	raw, err := sql.Open("sqlite", core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.Exec(`DELETE FROM sessions; DELETE FROM repos`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.RegistryPath(), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	return raw
}

func countRows(t *testing.T, raw *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// A write failing part-way through the import rolls back every imported row
// and the marker, fails Open, leaves the registry in place, and the next Open
// imports it in full. The injected failure stands in for a crash before the
// commit; this is not a process-kill test (SQLite's own rollback of an
// interrupted transaction is what makes a real crash equivalent).
func TestLegacyImportFailureRollsBackAndRetries(t *testing.T) {
	raw := seedEmptyStoreWithRegistry(t, twoWorkspaceRegistry)
	if _, err := raw.Exec(`CREATE TRIGGER fail_import BEFORE INSERT ON sessions WHEN NEW.id='boom'
		BEGIN SELECT RAISE(ABORT, 'injected import failure'); END`); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(); err == nil {
		db.Close()
		t.Fatal("Open succeeded although the legacy import failed")
	}
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("%d sessions survived a failed import", n)
	}
	if n := countRows(t, raw, `SELECT count(*) FROM repos`); n != 0 {
		t.Fatalf("%d repos survived a failed import", n)
	}
	if n := countRows(t, raw, `SELECT count(*) FROM legacy_registry_import`); n != 0 {
		t.Fatal("failed import was marked complete")
	}
	if _, err := os.Stat(core.RegistryPath()); err != nil {
		t.Fatalf("registry not left in place after a failed import: %v", err)
	}
	if _, done := migratedStores.Load(core.DBPath()); done {
		t.Fatal("Open recorded the store as migrated after a failed import")
	}

	if _, err := raw.Exec(`DROP TRIGGER fail_import`); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatalf("retry Open: %v", err)
	}
	db.Close()
	checkRegistryImportedOnce(t, "retry")
	if n := countRows(t, raw, `SELECT count(*) FROM sessions WHERE root_id='boom'`); n != 1 {
		t.Fatalf("second workspace has %d sub-sessions after retry, want 1", n)
	}
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(flags) })
	return &buf
}

// openInFreshProcess runs one Open in a child process (a restart) and returns
// its output.
func openInFreshProcess(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcessOpen$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperOpenEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh process Open: %v\n%s", err, out)
	}
	return string(out)
}

// Once the import has committed, moving the registry aside is housekeeping:
// a persistent rename failure is logged, never fails Open (in this process or
// later ones), never imports again, and the move is retried by each later
// process until it succeeds. The failed rename leaves the on-disk state a crash
// between commit and rename would; this is not a process-kill test.
func TestLegacyRegistryMoveFailureAfterCommitIsNonFatalAndRetried(t *testing.T) {
	raw := seedEmptyStoreWithRegistry(t, twoWorkspaceRegistry)
	logs := captureLog(t)
	backup := core.RegistryPath() + ".migrated"
	if err := os.MkdirAll(filepath.Join(backup, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatalf("Open failed on a committed import whose registry could not be moved: %v", err)
	}
	db.Close()
	if !strings.Contains(logs.String(), "is imported, but moving it to") {
		t.Fatalf("no diagnostic for the failed move; log = %q", logs.String())
	}
	if n := countRows(t, raw, `SELECT count(*) FROM legacy_registry_import`); n != 1 {
		t.Fatal("committed import has no marker")
	}
	if _, err := os.Stat(core.RegistryPath()); err != nil {
		t.Fatalf("registry missing after a failed move: %v", err)
	}
	if db, err = Open(); err != nil {
		t.Fatalf("second Open in the same process: %v", err)
	}
	db.Close()
	if testing.Short() {
		return
	}

	// The failure persists across a restart: still usable, still one import,
	// still reported.
	if out := openInFreshProcess(t); !strings.Contains(out, "is imported, but moving it to") {
		t.Fatalf("restart did not retry and report the move; output = %q", out)
	}
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`); n != 4 {
		t.Fatalf("store has %d sessions after a restart, want the 4 imported once", n)
	}

	// Once the obstacle is gone, the next restart completes the move.
	if err := os.RemoveAll(backup); err != nil {
		t.Fatal(err)
	}
	openInFreshProcess(t)
	if _, err := os.Stat(core.RegistryPath()); !os.IsNotExist(err) {
		t.Fatalf("registry still in place after the retried move: %v", err)
	}
	checkRegistryImportedOnce(t, "after the retried move")
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`); n != 4 {
		t.Fatalf("store has %d sessions, want the 4 imported once", n)
	}
}

// A registry that exists but cannot be read is an import failure, not an
// absent registry: Open fails, nothing is marked, and the next Open imports.
func TestLegacyRegistryReadFailureIsNotTreatedAsAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	raw := seedEmptyStoreWithRegistry(t, twoWorkspaceRegistry)
	if err := os.Chmod(core.RegistryPath(), 0); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(); err == nil {
		db.Close()
		t.Fatal("Open succeeded although the registry could not be read")
	}
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`) + countRows(t, raw, `SELECT count(*) FROM legacy_registry_import`); n != 0 {
		t.Fatal("unreadable registry left rows or a marker behind")
	}
	if err := os.Chmod(core.RegistryPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatalf("Open after the registry became readable: %v", err)
	}
	db.Close()
	checkRegistryImportedOnce(t, "after read failure")
}

// An unparseable registry does not block Open; it is left untouched and
// nothing is imported or marked. Open still records the store as migrated for
// this process, so its hot path stays read-only: a registry fixed later is
// imported by the next process's first Open (a restart), not by this process.
func TestUnreadableLegacyRegistryIsLeftInPlace(t *testing.T) {
	raw := seedEmptyStoreWithRegistry(t, `{not json`)
	logs := captureLog(t)
	db, err := Open()
	if err != nil {
		t.Fatalf("Open with an unreadable registry: %v", err)
	}
	db.Close()
	if !strings.Contains(logs.String(), "Nothing was imported. Fix or remove the file, then restart amux") {
		t.Fatalf("no diagnostic for the unreadable registry; log = %q", logs.String())
	}
	if b, err := os.ReadFile(core.RegistryPath()); err != nil || string(b) != `{not json` {
		t.Fatalf("registry changed: %q, %v", b, err)
	}
	if n := countRows(t, raw, `SELECT count(*) FROM legacy_registry_import`); n != 0 {
		t.Fatal("unreadable registry was marked imported")
	}

	if err := os.WriteFile(core.RegistryPath(), []byte(twoWorkspaceRegistry), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`); n != 0 {
		t.Fatalf("same process imported the fixed registry (%d sessions); expected it to wait for a restart", n)
	}

	if testing.Short() {
		return
	}
	openInFreshProcess(t)
	checkRegistryImportedOnce(t, "after restart")
}

// A registry is never merged into a store that already has sessions.
func TestLegacyRegistryIsNotImportedIntoAPopulatedStore(t *testing.T) {
	raw := seedEmptyStoreWithRegistry(t, twoWorkspaceRegistry)
	if _, err := raw.Exec(`INSERT INTO sessions(id,root_id,created) VALUES('existing','',1)`); err != nil {
		t.Fatal(err)
	}
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if n := countRows(t, raw, `SELECT count(*) FROM sessions`); n != 1 {
		t.Fatalf("store has %d sessions, want only the existing one", n)
	}
	if _, err := os.Stat(core.RegistryPath()); err != nil {
		t.Fatalf("registry moved although it was not imported: %v", err)
	}
}
