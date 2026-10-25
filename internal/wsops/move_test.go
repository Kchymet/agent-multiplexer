package wsops

import (
	"context"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// A move targets workgroups only: a repo home is refused by name, a workgroup
// that does not grant the agent's repo is refused, and `move` on the action
// path reports the workgroup it created so a client can open it.
func TestMoveAgentTargetsWorkgroupsOnly(t *testing.T) {
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	isolateStore(t)
	ctx := context.Background()
	db, _ := store.Open()
	for _, name := range []string{"api", "web"} {
		gitDir := bareRepoWithCommit(t)
		if err := db.PutRepo(store.Repo{Name: name, Source: gitDir, GitDir: gitDir}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if _, err := EnsureRepoHome("api"); err != nil {
		t.Fatal(err)
	}
	apiOnly, err := ApplyResult(ctx, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"repos": "api", "agent": "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	webOnly, err := ApplyResult(ctx, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"repos": "web"}})
	if err != nil {
		t.Fatal(err)
	}
	db, _ = store.Open()
	kids, _ := db.Children(apiOnly)
	db.Close()
	member := kids[0]

	if _, err := MoveAgent(ctx, member.ID, "api"); err == nil || !strings.Contains(err.Error(), "home session") {
		t.Fatalf("move into a repo home = %v, want a refusal naming the home", err)
	}
	if _, err := MoveAgent(ctx, member.ID, webOnly); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("move into a workgroup limited to another repo = %v", err)
	}
	if _, err := MoveAgent(ctx, member.ID, "nope"); err == nil || !strings.Contains(err.Error(), "amux workgroup ls") {
		t.Fatalf("move into an unknown workgroup = %v", err)
	}
	created, err := ApplyResult(ctx, core.Action{Action: core.ActionMove, ID: member.ID})
	if err != nil || created == "" {
		t.Fatalf("move to a new workgroup via the action path = %q, %v", created, err)
	}
	db, _ = store.Open()
	defer db.Close()
	moved, _, _ := db.GetSession(member.ID)
	grants, initialized, _ := db.CoordinatorRepoGrants(created)
	if moved.RootID != created || !initialized || strings.Join(grants, ",") != "api" {
		t.Fatalf("new workgroup %q: agent under %q, grants %v (%v)", created, moved.RootID, grants, initialized)
	}
	if same, err := MoveAgent(ctx, member.ID, created); err != nil || same != "" {
		t.Fatalf("move into the current workgroup = %q, %v; want a no-op", same, err)
	}
}
