package wsops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

func TestSingleRepoWorkgroupCeilingAcrossAddRescopeAndMove(t *testing.T) {
	isolateStore(t)
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	ctx := context.Background()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, repo := range []string{"api", "web"} {
		gitDir := bareRepoWithCommit(t)
		if err := db.PutRepo(store.Repo{Name: repo, Source: gitDir, GitDir: gitDir}); err != nil {
			t.Fatal(err)
		}
	}
	root, err := ApplyResult(ctx, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"repos": "api", "prompt": "implement the task", "coordinator": "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ := db.GetSession(root)
	members, _ := db.Children(root)
	if r.Role() != store.RoleCoordinator || r.Dir != store.CoordinatorDir(root) || r.Prompt != "implement the task" || len(members) != 1 || members[0].Prompt != "" {
		t.Fatalf("creation: %+v %+v", r, members)
	}
	member := members[0]
	if _, err := AddAgent(ctx, root, AgentSpec{Repos: []string{"api"}}); err != nil {
		t.Fatalf("second same-repo member: %v", err)
	}
	if _, err := AddAgent(ctx, root, AgentSpec{Repos: []string{"web"}}); err == nil {
		t.Fatal("add exceeded grant")
	}
	if err := SetAgentRepos(ctx, member.ID, []string{"api", "web"}); err == nil {
		t.Fatal("rescope exceeded grant")
	}
	if _, err := os.Stat(filepath.Join(member.Dir, "web")); !os.IsNotExist(err) {
		t.Fatalf("denied rescope created worktree: %v", err)
	}
	web := []string{"web"}
	target, err := createWorkspace(ctx, "web only", Coordinator{}, "", nil, &web)
	if err != nil {
		t.Fatal(err)
	}
	if err := MoveAgent(ctx, member.ID, target); err == nil {
		t.Fatal("move exceeded destination grant")
	}
	unchanged, _, _ := db.GetSession(member.ID)
	if !reflect.DeepEqual(unchanged, member) {
		t.Fatalf("denied move mutated member: %+v", unchanged)
	}
	if err := MoveAgent(ctx, member.ID, ""); err != nil {
		t.Fatal(err)
	}
	moved, _, _ := db.GetSession(member.ID)
	if moved.RootID == member.RootID {
		t.Fatal("move did not create a workgroup")
	}
	grants, initialized, err := db.CoordinatorRepoGrants(moved.RootID)
	if err != nil || !initialized || !reflect.DeepEqual(grants, []string{"api"}) {
		t.Fatalf("new destination ceiling: %v %v %v", grants, initialized, err)
	}
	moved.RootID = member.RootID
	if !reflect.DeepEqual(moved, member) {
		t.Fatalf("move changed identity/history fields: %+v", moved)
	}
}

func TestRetiredRepoAgentCannotCreateOrLaunch(t *testing.T) {
	isolateStore(t)
	if desc := core.DescriptorFor(core.ActionNewRepoAgent); desc.CreatesSession || desc.TargetsRoot {
		t.Fatalf("retired descriptor: %+v", desc)
	}
	id, err := ApplyResult(context.Background(), core.Action{Action: core.ActionNewRepoAgent, ID: "api"})
	if id != "" || err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("retired creation: %q %v", id, err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	roots, err := db.Roots()
	if err != nil || len(roots) != 0 {
		t.Fatalf("retired action persisted roots: %+v %v", roots, err)
	}
}
