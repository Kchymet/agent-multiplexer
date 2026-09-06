package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/store"
)

// Creation must launch the coordinator without any pane.open or follow-up start
// action, including when there are no members for a client to auto-attach to.
func TestCreateWorkgroupStartsCoordinator(t *testing.T) {
	for _, action := range []string{core.ActionNewWorkgroup, core.ActionCreateWorkspace} {
		for _, withMember := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/member=%v", action, withMember), func(t *testing.T) {
				d, eng := steerDaemon(t)
				fields := map[string]string{"name": "payments", "agent": "claude"}
				if withMember {
					fields["prompt"] = "fix the idempotency bug"
					fields["defaultAgent"] = "1"
				}
				r := d.handle(context.Background(), core.Action{Action: action, Fields: fields})
				if !r.OK || r.NewID == "" {
					t.Fatalf("create: %+v", r)
				}
				key := engine.Key{AgentID: r.NewID, Tab: panespec.TabAgent}
				inst, ok := eng.Lookup(key)
				if !ok || !inst.Alive() {
					t.Fatal("coordinator is not running after creation")
				}
				if keys := eng.ensuredKeys(); len(keys) != 1 || keys[0] != key {
					t.Fatalf("creation launched %v, want just the coordinator %v", keys, key)
				}

				db, err := store.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				root, ok, err := db.GetSession(r.NewID)
				if err != nil || !ok || root.Role() != store.RoleCoordinator {
					t.Fatalf("coordinator row: %+v, %v", root, err)
				}
				if root.Mode != store.ModeInteractive || root.Prompt != "" {
					t.Fatalf("coordinator should await input without a forced goal: %+v", root)
				}
				kids, err := db.Children(r.NewID)
				if err != nil {
					t.Fatal(err)
				}
				if withMember {
					if len(kids) != 1 || kids[0].Prompt != fields["prompt"] {
						t.Fatalf("first member lost its creation prompt: %+v", kids)
					}
				} else if len(kids) != 0 {
					t.Fatalf("empty workgroup unexpectedly has members: %+v", kids)
				}
			})
		}
	}
}

func TestCreateWorkgroupReportsCoordinatorLaunchFailure(t *testing.T) {
	d, eng := steerDaemon(t)
	eng.ensureErr = fmt.Errorf("runtime unavailable")
	r := d.handle(context.Background(), core.Action{
		Action: core.ActionNewWorkgroup, Fields: map[string]string{"name": "payments"},
	})
	if r.OK || r.NewID == "" || !strings.Contains(r.Error, "workgroup "+r.NewID+" created") ||
		!strings.Contains(r.Error, "runtime unavailable") || !strings.Contains(r.Error, "retry") {
		t.Fatalf("launch failure should identify the created workgroup and allow retry: %+v", r)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, err := db.GetSession(r.NewID); err != nil || !ok {
		t.Fatalf("created workgroup missing after launch failure: %v", err)
	}
	eng.ensureErr = nil
	if err := d.startAgent(context.Background(), r.NewID); err != nil {
		t.Fatalf("retry starting coordinator: %v", err)
	}
}

func TestFailedWorkgroupCreationDoesNotLaunchCoordinator(t *testing.T) {
	d, eng := steerDaemon(t)
	r := d.handle(context.Background(), core.Action{
		Action: core.ActionNewWorkgroup,
		Fields: map[string]string{"prompt": "go", "agent": "unknown-harness"},
	})
	if r.OK {
		t.Fatal("creation with an invalid harness succeeded")
	}
	if keys := eng.ensuredKeys(); len(keys) != 0 {
		t.Fatalf("failed creation launched sessions: %v", keys)
	}
}
