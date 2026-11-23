package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

// mkdirs creates each path (relative to base) as a directory.
func mkdirs(t *testing.T, base string, rel ...string) {
	t.Helper()
	for _, p := range rel {
		if err := os.MkdirAll(filepath.Join(base, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDrift(t *testing.T, got reconcileResult, orphans, missing, branches []string) {
	t.Helper()
	if got.Restricted {
		t.Fatalf("unexpected restricted result")
	}
	if !reflect.DeepEqual(got.OrphanDirs, orphans) {
		t.Errorf("OrphanDirs = %q, want %q", got.OrphanDirs, orphans)
	}
	if !reflect.DeepEqual(got.MissingDirs, missing) {
		t.Errorf("MissingDirs = %q, want %q", got.MissingDirs, missing)
	}
	if !reflect.DeepEqual(got.OrphanBranches, branches) {
		t.Errorf("OrphanBranches = %q, want %q", got.OrphanBranches, branches)
	}
}

// A workgroup in the current layout: the coordinator's own sandbox is the
// "coordinator" child of the workgroup container, beside its members. Neither
// that child nor the coordinator's private config homes are drift.
func TestReconcileNewCoordinatorLayout(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "r1/coordinator/.amux", "r1/coordinator/.claude", "r1/a1/repo")
	roots := []core.WorkgroupRow{{
		ID: "r1", Role: "coordinator", Dir: filepath.Join(s, "r1", "coordinator"),
		Agents: []core.AgentRow{{ID: "a1", Branch: "amux/r1-a1", Dir: filepath.Join(s, "r1", "a1")}},
	}}
	got := reconcile(s, roots, []branchRef{{Repo: "acme/api", Branch: "amux/r1-a1"}})
	assertDrift(t, got, nil, nil, nil)
}

// A legacy root owns its container outright (its stored Dir is sessions/<root>),
// so a non-agent subdirectory there — e.g. its own repo worktree — is the
// root's content, not an orphan. The same subdirectory under a current-layout
// workgroup (whose own dir is the coordinator child) is a genuine orphan.
func TestReconcileLegacyRootOwnsItsSubdirectories(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "old/agent-multiplexer", "old/.amux", "old/a1", "new/coordinator", "new/agent-multiplexer")
	roots := []core.WorkgroupRow{
		{ID: "old", Role: "coordinator", Dir: filepath.Join(s, "old"),
			Agents: []core.AgentRow{{ID: "a1", Dir: filepath.Join(s, "old", "a1")}}},
		{ID: "new", Role: "coordinator", Dir: filepath.Join(s, "new", "coordinator")},
	}
	assertDrift(t, reconcile(s, roots, nil), []string{"new/agent-multiplexer"}, nil, nil)
}

// Archiving keeps a session's row and its directory, so an archived workgroup's
// children (and their coordinator) still own their dirs; a vanished dir of an
// archived member is still reported, since it blocks a restore.
func TestReconcileArchivedWorkgroupChildren(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "wg/coordinator", "wg/a1", "wg/a2")
	roots := []core.WorkgroupRow{{
		ID: "wg", Role: "coordinator", Dir: filepath.Join(s, "wg", "coordinator"),
		Agents: []core.AgentRow{
			{ID: "a1", Archived: true, Dir: filepath.Join(s, "wg", "a1")},
			{ID: "a2", Archived: true, Dir: filepath.Join(s, "wg", "a2")},
			{ID: "a3", Archived: true, Dir: filepath.Join(s, "wg", "a3")},
		},
	}}
	missing := []string{"a3 (archived): " + filepath.Join(s, "wg", "a3") + " (does not exist)"}
	assertDrift(t, reconcile(s, roots, nil), nil, missing, nil)
}

// Directories no tracked session owns are flagged — inside a live workgroup
// and under a workgroup whose row is gone — as are branches mapping to no
// session; hidden dirs and stray files are ignored.
func TestReconcileGenuineOrphans(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "r1/coordinator", "r1/a1", "r1/a4", "r1/aX", "gone/a9", "harness/.amux")
	if err := os.WriteFile(filepath.Join(s, "r1", "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	roots := []core.WorkgroupRow{{
		ID: "r1", Role: "coordinator", Dir: filepath.Join(s, "r1", "coordinator"),
		Agents: []core.AgentRow{
			{ID: "a1", Branch: "amux/r1-a1", Dir: filepath.Join(s, "r1", "a1")},
			{ID: "a4", Dir: filepath.Join(s, "r1", "a4")},
		},
	}}
	disk := []branchRef{
		{Repo: "acme/api", Branch: "amux/r1-a1"},          // stored branch
		{Repo: "acme/api", Branch: "amux/r1-a4-describe"}, // blank stored branch, id matches
		{Repo: "acme/api", Branch: "amux/gone-a9"},        // deleted agent
	}
	assertDrift(t, reconcile(s, roots, disk),
		[]string{"gone/a9", "r1/aX"}, nil, []string{"acme/api · amux/gone-a9"})
}

// Missing dirs are judged by the exact stored path. A directory with the
// member's basename at a different path does not satisfy it (and is itself an
// orphan); a coordinator whose own dir vanished is reported too. A moved member
// whose stored dir still lives under its original workgroup is not drift.
func TestReconcileMissingDirs(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "r1/coordinator", "r9/coordinator", "r2/a2", "r2/a3", "r3")
	roots := []core.WorkgroupRow{
		{ID: "r1", Role: "coordinator", Dir: filepath.Join(s, "r1", "coordinator"),
			Agents: []core.AgentRow{{ID: "a2", Dir: filepath.Join(s, "r1", "a2")}}},
		{ID: "r3", Role: "coordinator", Dir: filepath.Join(s, "r3", "coordinator")},
		{ID: "r9", Role: "coordinator", Dir: filepath.Join(s, "r9", "coordinator"),
			Agents: []core.AgentRow{{ID: "a3", Dir: filepath.Join(s, "r2", "a3")}}}, // moved
	}
	missing := []string{
		"a2: " + filepath.Join(s, "r1", "a2") + " (does not exist)",
		"r3 (coordinator): " + filepath.Join(s, "r3", "coordinator") + " (does not exist)",
	}
	assertDrift(t, reconcile(s, roots, nil), []string{"r2/a2"}, missing, nil)
}

// A member row recorded before sessions stored their directory falls back to
// matching its id anywhere under the sessions dir.
func TestReconcileLegacyMemberWithoutStoredDir(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "r2/a1")
	roots := []core.WorkgroupRow{{ID: "r1", Agents: []core.AgentRow{{ID: "a1"}, {ID: "a2"}}}}
	missing := []string{"a2: no directory named a2 under " + s}
	assertDrift(t, reconcile(s, roots, nil), nil, missing, nil)
}

// A scoped (agent-principal) view hides physical paths. Reconciling it would
// flag every coordinator dir and invisible session, so it is reported as
// undeterminable instead of as drift.
func TestReconcileRestrictedViewIsNotDrift(t *testing.T) {
	s := t.TempDir()
	mkdirs(t, s, "r1/coordinator", "r1/a1")
	got := reconcile(s, []core.WorkgroupRow{{ID: "r1", Role: "coordinator", Agents: []core.AgentRow{{ID: "a1"}}}}, nil)
	if !got.Restricted || !got.empty() {
		t.Fatalf("restricted view = %+v, want Restricted with no drift", got)
	}
}

// TestReconcileFreshSessionsHaveNoDrift is the recurrence guard: sessions made
// by the real creation helpers — a workgroup with its coordinator, members, a
// moved member, an archived member and an archived workgroup — must reconcile
// with zero drift. The rows are built from the store only here, in the test, as
// the daemon's QuerySessions projection does (stored Dir, archived flag); the
// test never spells out a path itself, so a layout change in the helpers is
// exercised rather than mirrored.
func TestReconcileFreshSessionsHaveNoDrift(t *testing.T) {
	sandboxCLI(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
	t.Setenv("AMUX_JAIL", "off")
	ctx := context.Background()

	wg, err := wsops.CreateWorkspace(ctx, "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	var members []store.Session
	for i := 0; i < 3; i++ {
		m, err := wsops.AddAgent(ctx, wg, wsops.AgentSpec{Agent: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, m)
	}
	if err := wsops.SetArchived(ctx, members[1].ID, true); err != nil {
		t.Fatal(err)
	}
	other, err := wsops.CreateWorkspace(ctx, "billing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wsops.MoveAgent(ctx, members[2].ID, other); err != nil {
		t.Fatal(err)
	}
	done, err := wsops.CreateWorkspace(ctx, "shipped", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wsops.AddAgent(ctx, done, wsops.AgentSpec{Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := wsops.SetArchived(ctx, done, true); err != nil {
		t.Fatal(err)
	}

	rows := sessionRowsFromStore(t)
	coordinators := 0
	for _, r := range rows {
		if r.Role == store.RoleCoordinator {
			coordinators++
		}
	}
	if coordinators != 3 {
		t.Fatalf("coordinators = %d, want 3 (rows %+v)", coordinators, rows)
	}
	if got := reconcile(core.SessionsDir(), rows, nil); got.Restricted || !got.empty() {
		t.Fatalf("freshly created sessions drift: %+v", got)
	}

	// The guard must be able to fail: losing a coordinator's own dir is drift.
	coordDir := ""
	for _, r := range rows {
		if r.ID == wg {
			coordDir = r.Dir
		}
	}
	if err := os.RemoveAll(coordDir); err != nil {
		t.Fatal(err)
	}
	got := reconcile(core.SessionsDir(), rows, nil)
	if len(got.MissingDirs) != 1 || !strings.HasPrefix(got.MissingDirs[0], wg+" (coordinator): ") {
		t.Fatalf("removed coordinator dir not reported: %+v", got)
	}
}

// sessionRowsFromStore builds QuerySessions-shaped rows straight from the store.
// Test-only: production doctor reads these rows from the daemon.
func sessionRowsFromStore(t *testing.T) []core.WorkgroupRow {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	roots, err := db.Roots()
	if err != nil {
		t.Fatal(err)
	}
	var rows []core.WorkgroupRow
	for _, r := range roots {
		row := core.WorkgroupRow{ID: r.ID, Role: r.Role(), Dir: r.Dir}
		if row.Role != "" && row.Dir == "" {
			t.Fatalf("root %s (%s) was created without a stored dir", r.ID, row.Role)
		}
		kids, err := db.Children(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range kids {
			if k.Dir == "" {
				t.Fatalf("member %s was created without a stored dir", k.ID)
			}
			row.Agents = append(row.Agents, core.AgentRow{ID: k.ID, Branch: k.Branch, Dir: k.Dir, Archived: k.Archived})
		}
		rows = append(rows, row)
	}
	return rows
}
