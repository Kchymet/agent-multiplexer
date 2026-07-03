package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/core"
)

// TestReconcile drives the pure reconciliation core: it flags worktree dirs and
// branches with no session and sessions with no dir, while recognizing a moved
// agent (dir under its original root) by agent id rather than mis-flagging it.
func TestReconcile(t *testing.T) {
	sessions := t.TempDir()
	// On disk: r1/{a1,aX}, and r2/a3 (a3 was moved to workgroup r9 but its dir
	// still lives under its original root r2).
	for _, p := range []string{"r1/a1", "r1/aX", "r2/a3", "r9/a4"} {
		if err := os.MkdirAll(filepath.Join(sessions, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file (not a dir) under a root must be ignored.
	if err := os.WriteFile(filepath.Join(sessions, "r1", "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots := []core.WorkgroupRow{
		{ID: "r1", Agents: []core.AgentRow{
			{ID: "a1", Branch: "amux/r1-a1"},
			{ID: "a2", Branch: "amux/r1-a2"}, // known, but no dir on disk
		}},
		{ID: "r9", Agents: []core.AgentRow{
			{ID: "a3", Branch: "amux/r2-a3"}, // moved out of r2; dir still under r2
			{ID: "a4", Branch: ""},           // live agent with a blank stored branch
		}},
	}
	disk := []branchRef{
		{Repo: "acme/api", Branch: "amux/r1-a1"},  // known via stored branch
		{Repo: "acme/api", Branch: "amux/r9-a4"},  // known via agent id (blank stored branch)
		{Repo: "acme/api", Branch: "amux/gone-x"}, // orphan (deleted agent)
	}

	orphanDirs, missingDirs, orphanBranches := reconcile(sessions, roots, disk)

	if want := []string{"r1/aX"}; !reflect.DeepEqual(orphanDirs, want) {
		t.Errorf("orphanDirs = %v, want %v", orphanDirs, want)
	}
	if want := []string{"a2"}; !reflect.DeepEqual(missingDirs, want) {
		t.Errorf("missingDirs = %v, want %v", missingDirs, want)
	}
	if want := []string{"acme/api · amux/gone-x"}; !reflect.DeepEqual(orphanBranches, want) {
		t.Errorf("orphanBranches = %v, want %v", orphanBranches, want)
	}
}

// TestReconcileClean verifies no drift is reported when store and disk agree.
func TestReconcileClean(t *testing.T) {
	sessions := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessions, "r1", "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []core.WorkgroupRow{{ID: "r1", Agents: []core.AgentRow{{ID: "a1", Branch: "amux/r1-a1"}}}}
	disk := []branchRef{{Repo: "acme/api", Branch: "amux/r1-a1"}}

	od, md, ob := reconcile(sessions, roots, disk)
	if len(od)+len(md)+len(ob) != 0 {
		t.Errorf("expected no drift, got dirs=%v missing=%v branches=%v", od, md, ob)
	}
}
