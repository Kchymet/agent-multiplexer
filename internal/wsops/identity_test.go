package wsops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// identityFixture is an isolated store with tracked repo acme and a
// workgroup root granted it.
func identityFixture(t *testing.T) (string, store.Identity) {
	t.Helper()
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	isolateStore(t)
	gitDir := bareRepoWithCommit(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutRepo(store.Repo{Name: "acme", Source: gitDir, GitDir: gitDir}); err != nil {
		t.Fatal(err)
	}
	rootID := "wg"
	root, err := db.CreateSession(store.Session{ID: rootID, Scope: store.ScopeWork, Mode: store.ModeTask,
		Dir: store.CoordinatorDir(rootID)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCoordinatorRepoGrants(rootID, []string{"acme"}); err != nil {
		t.Fatal(err)
	}
	return rootID, root
}

func addAgentAction(root string) core.Action {
	return core.Action{Action: core.ActionAddAgent, ID: root, Fields: map[string]string{"agent": "codex", "repos": "acme"}}
}

// ApplyIdentity returns the identity CreateSession produced, and the legacy
// ApplyResult still returns only the id.
func TestApplyIdentityAddAgentAndLegacy(t *testing.T) {
	rootID, root := identityFixture(t)
	ctx := context.Background()
	ident, err := ApplyIdentity(ctx, addAgentAction(rootID), root.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if !ident.Complete() || ident.RootID != rootID || ident.RootIncarnation != root.Incarnation {
		t.Fatalf("identity = %+v", ident)
	}
	db, _ := store.Open()
	cur, _ := db.SessionIdentities([]string{ident.ID})
	db.Close()
	if cur[ident.ID].Incarnation != ident.Incarnation {
		t.Fatal("returned incarnation is not the stored one")
	}
	id, err := ApplyResult(ctx, addAgentAction(rootID))
	if err != nil || id == "" || id == ident.ID {
		t.Fatalf("legacy add-agent = %q, %v", id, err)
	}
	// new-workgroup: the identity is the new root's own.
	wg, err := ApplyIdentity(ctx, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"name": "x"}}, "")
	if err != nil || wg.RootID != wg.ID || wg.RootIncarnation != wg.Incarnation {
		t.Fatalf("new-workgroup identity = %+v, %v", wg, err)
	}
	// An expected root on a root creation, or any non-creation verb, refuses.
	if _, err := ApplyIdentity(ctx, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{}}, "x"); !errors.Is(err, store.ErrRootChanged) {
		t.Fatalf("new-workgroup with expectRoot = %v", err)
	}
	if _, err := ApplyIdentity(ctx, core.Action{Action: core.ActionRename, ID: rootID, Fields: map[string]string{"name": "n"}}, ""); err == nil {
		t.Fatal("rename through ApplyIdentity accepted")
	}
}

// A forged or stale expected root is refused before anything is prepared.
func TestApplyIdentityEarlyRootRefusal(t *testing.T) {
	rootID, _ := identityFixture(t)
	before, _ := os.ReadDir(store.RootDir(rootID))
	if _, err := ApplyIdentity(context.Background(), addAgentAction(rootID), "00000000000000000000000000000000"); !errors.Is(err, store.ErrRootChanged) {
		t.Fatalf("forged root = %v", err)
	}
	after, _ := os.ReadDir(store.RootDir(rootID))
	if len(after) != len(before) {
		t.Fatalf("preparation ran before the early refusal: %d -> %d entries", len(before), len(after))
	}
}

// The root replaced during preparation: the atomic check refuses the insert
// and the prepared worktree, private Git dir, layout record and agent dir are
// removed; nothing else under the root is touched.
func TestApplyIdentityRootReplacedDuringPreparation(t *testing.T) {
	rootID, root := identityFixture(t)
	keep := filepath.Join(store.RootDir(rootID), "unrelated")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	var prepared store.Session
	beforeInsert = func(a store.Session) {
		prepared = a
		db, _ := store.Open()
		defer db.Close()
		if err := db.DeleteSession(rootID); err != nil {
			t.Error(err)
		}
		if _, err := db.CreateSession(store.Session{ID: rootID, Scope: store.ScopeWork}, ""); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeInsert = func(store.Session) {} })
	_, err := ApplyIdentity(context.Background(), addAgentAction(rootID), root.Incarnation)
	if !errors.Is(err, store.ErrRootChanged) {
		t.Fatalf("replaced root = %v", err)
	}
	if prepared.ID == "" {
		t.Fatal("preparation did not reach the insert")
	}
	if _, err := os.Lstat(prepared.Dir); !os.IsNotExist(err) {
		t.Fatalf("prepared agent dir survived: %v", err)
	}
	if _, err := os.Lstat(gitLayoutPath(prepared.ID, "acme")); !os.IsNotExist(err) {
		t.Fatalf("prepared layout record survived: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated directory removed: %v", err)
	}
	db, _ := store.Open()
	defer db.Close()
	if _, ok, _ := db.GetSession(prepared.ID); ok {
		t.Fatal("refused member recorded")
	}
}

// Two creations choosing the same free id: the second can never adopt (or
// later remove) the first one's directory. Its exclusive mkdir fails, it takes
// another id, and the first one's row and files are intact.
func TestAgentIDCollisionNeverSharesDirectory(t *testing.T) {
	rootID, root := identityFixture(t)
	winnerDir := store.AgentDir(rootID, "dup")
	if err := os.MkdirAll(winnerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(winnerDir, "winner-work")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids := []string{"dup", "other"}
	newAgentID = func(*store.DB) string { id := ids[0]; ids = ids[1:]; return id }
	t.Cleanup(func() { newAgentID = func(db *store.DB) string { return db.NewID() } })
	ident, err := ApplyIdentity(context.Background(), addAgentAction(rootID), root.Incarnation)
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != "other" {
		t.Fatalf("collided id adopted: %q", ident.ID)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("occupied directory touched: %v", err)
	}
}

// The loser of an insert race (the id's row appears after preparation) gets
// ErrSessionExists and removes only its own exclusively created directory:
// the winner's row and files survive.
func TestInsertRaceLoserRemovesOnlyItsOwn(t *testing.T) {
	rootID, root := identityFixture(t)
	newAgentID = func(*store.DB) string { return "race" }
	t.Cleanup(func() { newAgentID = func(db *store.DB) string { return db.NewID() } })
	winnerMarker := filepath.Join(t.TempDir(), "winner")
	beforeInsert = func(a store.Session) {
		db, _ := store.Open()
		defer db.Close()
		// The winner's row lands first (its files live elsewhere).
		if _, err := db.CreateSession(store.Session{ID: "race", RootID: rootID, Dir: filepath.Dir(winnerMarker)}, ""); err != nil {
			t.Error(err)
		}
		os.WriteFile(winnerMarker, []byte("x"), 0o644)
	}
	t.Cleanup(func() { beforeInsert = func(store.Session) {} })
	_, err := ApplyIdentity(context.Background(), addAgentAction(rootID), root.Incarnation)
	if !errors.Is(err, store.ErrSessionExists) {
		t.Fatalf("loser = %v", err)
	}
	if _, err := os.Lstat(store.AgentDir(rootID, "race")); !os.IsNotExist(err) {
		t.Fatalf("loser's own dir survived: %v", err)
	}
	if _, err := os.Stat(winnerMarker); err != nil {
		t.Fatal("winner's files removed")
	}
	db, _ := store.Open()
	defer db.Close()
	if s, ok, _ := db.GetSession("race"); !ok || s.Dir != filepath.Dir(winnerMarker) {
		t.Fatalf("winner row changed: %+v %v", s, ok)
	}
}
