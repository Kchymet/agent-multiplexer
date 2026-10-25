package wsops

import (
	"context"
	"reflect"
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

func TestMoveAgentArchiveFencesBeforeTargetCreation(t *testing.T) {
	for _, tc := range []struct {
		name                                           string
		sourceArchived, targetArchived, memberArchived bool
		target, wantError                              string
	}{
		{name: "archived source existing target", sourceArchived: true, target: "target", wantError: `restore workgroup "source" before moving member "member"`},
		{name: "archived source new target", sourceArchived: true, wantError: `restore workgroup "source" before moving member "member"`},
		{name: "archived target remains denied", targetArchived: true, target: "target", wantError: "not an active workgroup"},
		{name: "completed member archived target remains denied", memberArchived: true, targetArchived: true, target: "target", wantError: "not an active workgroup"},
		{name: "completed member may move between active groups", memberArchived: true, target: "target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateStore(t)
			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var archivedAt int64
			if tc.memberArchived {
				archivedAt = 77
			}
			for _, s := range []store.Session{
				{ID: "source", Scope: store.ScopeWork, Archived: tc.sourceArchived},
				{ID: "target", Scope: store.ScopeWork, Archived: tc.targetArchived},
				{ID: "member", RootID: "source", Archived: tc.memberArchived, ArchivedAt: archivedAt, Branch: "amux/source-member", ClaudeID: "preserved-history"},
			} {
				if err := db.PutSession(s); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{"source", "target"} {
				if err := db.SetCoordinatorRepoGrants(id, nil); err != nil {
					t.Fatal(err)
				}
			}
			before, err := db.AllSessions()
			if err != nil {
				t.Fatal(err)
			}
			created, err := ApplyResult(context.Background(), core.Action{Action: core.ActionMove, ID: "member", Target: tc.target})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || created != "" {
					t.Fatalf("move returned %q, %v; want %s", created, err, tc.wantError)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after, err := db.AllSessions()
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError == "" {
				for i := range before {
					if before[i].ID == "member" {
						before[i].RootID = "target"
					}
				}
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("move unexpectedly changed membership, history, or created a target: before=%+v after=%+v", before, after)
			}
		})
	}
}
