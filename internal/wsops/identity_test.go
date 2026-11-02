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
	winnerLayout := gitLayoutPath("race", "acme")
	beforeInsert = func(a store.Session) {
		db, _ := store.Open()
		defer db.Close()
		// A writer that does not reserve ids lands the same id first, with its
		// own files and its own id-keyed layout record.
		if _, err := db.CreateSession(store.Session{ID: "race", RootID: rootID, Dir: filepath.Dir(winnerMarker)}, ""); err != nil {
			t.Error(err)
		}
		os.WriteFile(winnerMarker, []byte("x"), 0o644)
		os.WriteFile(winnerLayout, []byte("winner layout"), 0o600)
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
	if b, err := os.ReadFile(winnerLayout); err != nil || string(b) != "winner layout" {
		t.Fatalf("id-keyed layout record of the row owner removed or changed: %q %v", b, err)
	}
	db, _ := store.Open()
	defer db.Close()
	if s, ok, _ := db.GetSession("race"); !ok || s.Dir != filepath.Dir(winnerMarker) {
		t.Fatalf("winner row changed: %+v %v", s, ok)
	}
}

// Two creations in different workgroups that mint the same id: the id is
// reserved globally, so the second never prepares it; when the first is then
// refused (its root replaced) its owned cleanup removes only its own records,
// and the second session's directory, config home and layout record survive.
func TestSameIDAcrossRootsNeverShared(t *testing.T) {
	rootID, root := identityFixture(t)
	db, _ := store.Open()
	other, err := db.CreateSession(store.Session{ID: "wg2", Scope: store.ScopeWork, Dir: store.CoordinatorDir("wg2")}, "")
	if err == nil {
		err = db.SetCoordinatorRepoGrants("wg2", []string{"acme"})
	}
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"dup", "dup", "next"}
	newAgentID = func(*store.DB) string { id := ids[0]; ids = ids[1:]; return id }
	t.Cleanup(func() { newAgentID = func(db *store.DB) string { return db.NewID() } })
	var second store.Identity
	var secondErr error
	beforeInsert = func(a store.Session) {
		if a.RootID != rootID {
			return
		}
		beforeInsert = func(store.Session) {}
		// While the first holds "dup", the second creation (another root)
		// mints "dup" too and must take another id.
		second, secondErr = ApplyIdentity(context.Background(), addAgentAction("wg2"), other.Incarnation)
		// Then the first's root is replaced, so it is refused late.
		db, _ := store.Open()
		defer db.Close()
		db.DeleteSession(rootID)
		db.CreateSession(store.Session{ID: rootID, Scope: store.ScopeWork}, "")
	}
	t.Cleanup(func() { beforeInsert = func(store.Session) {} })
	if _, err := ApplyIdentity(context.Background(), addAgentAction(rootID), root.Incarnation); !errors.Is(err, store.ErrRootChanged) {
		t.Fatalf("first = %v", err)
	}
	if secondErr != nil || second.ID != "next" {
		t.Fatalf("second = %+v, %v", second, secondErr)
	}
	db, _ = store.Open()
	defer db.Close()
	s, ok, _ := db.GetSession("next")
	if !ok {
		t.Fatal("second session missing")
	}
	for _, p := range []string{s.Dir, filepath.Join(s.Dir, "acme", ".git"), gitLayoutPath("next", "acme")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("second session's %s removed: %v", p, err)
		}
	}
	if _, err := os.Lstat(gitLayoutPath("dup", "acme")); !os.IsNotExist(err) {
		t.Fatalf("refused creation's own layout record survived: %v", err)
	}
	if _, err := os.Lstat(store.AgentDir(rootID, "dup")); !os.IsNotExist(err) {
		t.Fatalf("refused creation's own directory survived: %v", err)
	}
	if entries, _ := os.ReadDir(reservationDir()); len(entries) != 0 {
		t.Fatalf("reservations leaked: %d", len(entries))
	}
}
