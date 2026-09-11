package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/core"
)

func TestCoordinatorGrantEmptyAndUninitializedRemainDistinctFromChildHistory(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(Repo{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"explicit-empty", "legacy-blank"} {
		if err := db.PutSession(Session{ID: id, Scope: ScopeWork}); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSession(Session{ID: id + "-child", RootID: id, Repo: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetCoordinatorRepoGrants("explicit-empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if repos, initialized, err := db.CoordinatorRepoGrants("explicit-empty"); err != nil || !initialized || len(repos) != 0 {
		t.Fatalf("explicit empty grants = %v, initialized=%v, err=%v", repos, initialized, err)
	}
	if repos, initialized, err := db.CoordinatorRepoGrants("legacy-blank"); err != nil || initialized || len(repos) != 0 {
		t.Fatalf("legacy blank grants = %v, initialized=%v, err=%v", repos, initialized, err)
	}
}

func TestCoordinatorGrantMigrationUsesPersistedRootOnly(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", "file:"+core.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
CREATE TABLE repos (name TEXT PRIMARY KEY, source TEXT NOT NULL, git_dir TEXT NOT NULL);
CREATE TABLE sessions (
 id TEXT PRIMARY KEY, root_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '',
 agent TEXT NOT NULL DEFAULT 'claude', model TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT 'task',
 repo TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '', dir TEXT NOT NULL DEFAULT '',
 claude_id TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', created INTEGER NOT NULL DEFAULT 0,
 scope TEXT NOT NULL DEFAULT 'work', archived INTEGER NOT NULL DEFAULT 0, archived_at INTEGER NOT NULL DEFAULT 0);
INSERT INTO repos(name,source,git_dir) VALUES('api','',''),('web','','');
INSERT INTO sessions(id,repo,scope) VALUES('persisted','web,api','work'),('blank','','work');
INSERT INTO sessions(id,root_id,repo) VALUES('child','blank','api');
PRAGMA user_version=1;`)
	if closeErr := raw.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}

	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if repos, initialized, err := db.CoordinatorRepoGrants("persisted"); err != nil || !initialized || !reflect.DeepEqual(repos, []string{"api", "web"}) {
		t.Fatalf("persisted migration = %v, initialized=%v, err=%v", repos, initialized, err)
	}
	if repos, initialized, err := db.CoordinatorRepoGrants("blank"); err != nil || initialized || len(repos) != 0 {
		t.Fatalf("child-derived migration = %v, initialized=%v, err=%v", repos, initialized, err)
	}
}

func TestOpenReadOnlyNeverMigratesOrWrites(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(Repo{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(Session{ID: "wg", Scope: ScopeWork}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCoordinatorRepoGrants("wg", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	read, err := OpenReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if repos, initialized, err := read.CoordinatorRepoGrants("wg"); err != nil || !initialized || !reflect.DeepEqual(repos, []string{"api"}) {
		t.Fatalf("read-only grants = %v, initialized=%v, err=%v", repos, initialized, err)
	}
	if err := read.SetCoordinatorRepoGrants("wg", nil); err == nil {
		t.Fatal("read-only store accepted a grant mutation")
	}
	if _, err := os.Stat(core.DBPath()); err != nil {
		t.Fatalf("read-only open lost the existing database: %v", err)
	}
}
