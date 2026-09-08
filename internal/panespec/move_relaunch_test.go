package panespec

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"amux/internal/store"
	"amux/internal/wsops"
)

// Moving changes policy membership only. Relaunch must use the authoritative
// stored physical directory even after the old root row is gone; reads must not
// derive newRoot/id, relocate the checkout, or discard private files.
func TestMovedMemberRelaunchUsesStoredOwnDirAfterOldRootRemoval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	useFakeSecureBwrap(t)
	ctx := context.Background()
	oldRoot, err := wsops.CreateWorkspace(ctx, "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	member, err := wsops.AddAgent(ctx, oldRoot, wsops.AgentSpec{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	storedDir := member.Dir
	private := filepath.Join(storedDir, "private-state")
	if err := os.WriteFile(private, []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	newRoot, err := wsops.CreateWorkspace(ctx, "new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := wsops.MoveAgent(ctx, member.ID, newRoot); err != nil {
		t.Fatal(err)
	}
	if err := wsops.DeleteByID(ctx, oldRoot); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	moved, ok, err := db.GetSession(member.ID)
	if err != nil || !ok {
		t.Fatalf("moved member missing: %v", err)
	}
	if _, ok, err := db.GetSession(oldRoot); err != nil || ok {
		t.Fatalf("old root row still present: ok=%v err=%v", ok, err)
	}
	_ = db.Close()
	if moved.RootID != newRoot || moved.Dir != storedDir {
		t.Fatalf("move changed physical identity: %+v, old dir %q new root %q", moved, storedDir, newRoot)
	}
	if got, err := os.ReadFile(private); err != nil || string(got) != "preserved" {
		t.Fatalf("private filesystem was not preserved: %q, %v", got, err)
	}
	spec := testLaunchSpec(t, moved)
	dir, _, argv, err := Resolve(spec, TabTerminal)
	if err != nil {
		t.Fatal(err)
	}
	if dir != storedDir || argvSequence(argv, "--bind", storedDir, storedDir) < 0 {
		t.Fatalf("relaunch did not bind stored own dir: dir=%q argv=%v", dir, argv)
	}
	if slices.Contains(argv, store.RootDir(newRoot)) || slices.Contains(argv, store.RootDir(oldRoot)) {
		t.Fatalf("relaunch exposed a membership ancestor: %v", argv)
	}
}
