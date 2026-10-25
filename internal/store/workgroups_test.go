package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRepoWorkgroupMigrationPreservesMembersAndHistory(t *testing.T) {
	db := openTemp(t)
	if err := db.PutRepo(Repo{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(Repo{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	home := Session{ID: "api", Scope: ScopeRepo, Repo: "api", Agent: "claude", Dir: RootDir("api"), ClaudeID: "home-history"}
	if err := db.PutSession(home); err != nil {
		t.Fatal(err)
	}
	var members []Session
	for _, id := range []string{"active", "archived", "removed-repo"} {
		repo := "api"
		if id == "removed-repo" {
			repo = "removed"
		}
		root := Session{ID: id, Scope: ScopeRepo, Repo: repo, Name: "old " + id, Created: 10, Archived: id == "archived", ArchivedAt: 99}
		if err := db.PutSession(root); err != nil {
			t.Fatal(err)
		}
		member := Session{ID: id + "-member", RootID: id, Agent: "codex", Mode: ModeTask, Repo: repo,
			Dir: AgentDir(id, id+"-member"), Branch: "amux/" + id, ClaudeID: id + "-conversation", Prompt: "original task",
			Created: 11, Archived: root.Archived, ArchivedAt: root.ArchivedAt}
		if err := os.MkdirAll(member.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(member.Dir, "history.jsonl"), []byte("preserved history"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSession(member); err != nil {
			t.Fatal(err)
		}
		members = append(members, member)
	}
	// A read-only policy lookup cannot provision or promote legacy sessions.
	ro, err := OpenReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := ro.GetSession("active")
	ro.Close()
	if old.Scope != ScopeRepo {
		t.Fatal("read-only open promoted a root")
	}
	if _, err := os.Stat(CoordinatorDir("active")); !os.IsNotExist(err) {
		t.Fatalf("read provisioned directory: %v", err)
	}

	// The older grant migration already ran when openTemp opened this database.
	// Promotion must initialize its own grants and remain stable on a second open.
	var promoted map[string]Session
	for pass := 0; pass < 2; pass++ {
		upgraded, err := Open()
		if err != nil {
			t.Fatal(err)
		}
		for _, member := range members {
			got, _, _ := upgraded.GetSession(member.ID)
			if !reflect.DeepEqual(got, member) {
				t.Fatalf("member changed: got %+v want %+v", got, member)
			}
			b, err := os.ReadFile(filepath.Join(member.Dir, "history.jsonl"))
			if err != nil || string(b) != "preserved history" {
				t.Fatalf("history: %q %v", b, err)
			}
			root, _, _ := upgraded.GetSession(member.RootID)
			if root.Role() != RoleCoordinator || root.Dir != CoordinatorDir(root.ID) || root.Name != "old "+root.ID || root.Created != 10 || root.Archived != member.Archived || root.ArchivedAt != member.ArchivedAt {
				t.Fatalf("promoted root: %+v", root)
			}
			if root.ClaudeID == "" || root.Agent != "claude" {
				t.Fatalf("missing coordinator runtime: %+v", root)
			}
			grants, initialized, err := upgraded.CoordinatorRepoGrants(root.ID)
			want := []string{"api"}
			if root.ID == "removed-repo" {
				want = nil
			}
			if err != nil || !initialized || !reflect.DeepEqual(grants, want) {
				t.Fatalf("grants: %v %v %v", grants, initialized, err)
			}
			if pass == 0 {
				if promoted == nil {
					promoted = map[string]Session{}
				}
				promoted[root.ID] = root
			} else if !reflect.DeepEqual(promoted[root.ID], root) {
				t.Fatalf("second migration changed root: %+v", root)
			}
		}
		gotHome, _, _ := upgraded.GetSession(home.ID)
		if !reflect.DeepEqual(gotHome, home) {
			t.Fatalf("repo home changed: %+v", gotHome)
		}
		upgraded.Close()
	}
}

func TestRepoWorkgroupMigrationRejectsCoordinatorAliasWithoutChangingRows(t *testing.T) {
	db := openTemp(t)
	if err := db.PutSession(Session{ID: "legacy", Scope: ScopeRepo, Repo: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(RootDir("legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, CoordinatorDir("legacy")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	if upgraded, err := Open(); err == nil {
		upgraded.Close()
		t.Fatal("migration accepted coordinator symlink")
	}
	if version, err := db.SchemaVersion(); err != nil || version != 2 {
		t.Fatalf("failed migration marked schema complete: %d %v", version, err)
	}
	root, _, _ := db.GetSession("legacy")
	if root.Scope != ScopeRepo {
		t.Fatal("failed migration changed legacy root")
	}
	if _, initialized, _ := db.CoordinatorRepoGrants(root.ID); initialized {
		t.Fatal("failed migration initialized grants")
	}
}
