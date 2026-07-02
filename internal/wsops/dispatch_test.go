package wsops

import (
	"context"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// firstChild returns the id of a workgroup root's single agent, for tests that
// create a one-agent workgroup and then target that agent.
func firstChild(t *testing.T, rootID string) string {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	kids, err := db.Children(rootID)
	if err != nil || len(kids) == 0 {
		t.Fatalf("no children under %s (err=%v)", rootID, err)
	}
	return kids[0].ID
}

// exists reports whether a session id is still in the store.
func exists(t *testing.T, id string) bool {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, ok, _ := db.GetSession(id)
	return ok
}

// TestDispatchParity is the AGE-132 parity guard: every lifecycle verb runs
// through wsops.Dispatch — the ONE path the daemon, the mux server, and the
// CLI-via-daemon all funnel into — and both its engine and store outcomes are
// asserted. The engine-stop decision comes from a single descriptor table, so all
// three paths stop the target's process for exactly the same verbs. This is what
// makes `amux wg archive` (set-archived) stop the engine like the TUI's one-key
// archive — the drift that motivated the issue: before, the daemon stopped the
// engine for "archive" but not "set-archived".
func TestDispatchParity(t *testing.T) {
	cases := []struct {
		action   string
		wantStop bool
		fields   map[string]string
		// check asserts the store outcome for the targeted agent after Dispatch.
		check func(t *testing.T, agentID string)
	}{
		{
			action: core.ActionDelete, wantStop: true,
			check: func(t *testing.T, id string) {
				if exists(t, id) {
					t.Error("delete: agent still in store")
				}
			},
		},
		{
			action: core.ActionKill, wantStop: true,
			check: func(t *testing.T, id string) {
				if exists(t, id) {
					t.Error("kill: agent still in store")
				}
			},
		},
		{
			action: core.ActionArchive, wantStop: true, // TUI one-key toggle
			check: func(t *testing.T, id string) {
				if !getSession(t, id).Archived {
					t.Error("archive: agent not archived")
				}
			},
		},
		{
			action: core.ActionSetArchived, wantStop: true, // CLI explicit — the fixed verb
			fields: map[string]string{"archived": "true"},
			check: func(t *testing.T, id string) {
				if !getSession(t, id).Archived {
					t.Error("set-archived: agent not archived")
				}
			},
		},
		{
			action: core.ActionRename, wantStop: false,
			fields: map[string]string{"name": "renamed"},
			check: func(t *testing.T, id string) {
				if got := getSession(t, id).Name; got != "renamed" {
					t.Errorf("rename: name = %q, want renamed", got)
				}
			},
		},
		{
			action: core.ActionAgentSetRepos, wantStop: false,
			fields: map[string]string{"repos": ""},
			check: func(t *testing.T, id string) {
				if got := getSession(t, id).Repo; got != "" {
					t.Errorf("agent-set-repos: repo = %q, want empty", got)
				}
			},
		},
		{
			action: core.ActionRefresh, wantStop: false,
			check: func(t *testing.T, id string) {
				if !exists(t, id) {
					t.Error("refresh: agent unexpectedly gone")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			isolateStore(t)
			ctx := context.Background()
			rootID, err := CreateWorkspace(ctx, "ws", &AgentSpec{})
			if err != nil {
				t.Fatal(err)
			}
			agentID := firstChild(t, rootID)

			// The descriptor is the single source of the engine-stop decision.
			if core.DescriptorFor(tc.action).StopsEngine != tc.wantStop {
				t.Fatalf("descriptor drift: DescriptorFor(%s).StopsEngine = %v, want %v",
					tc.action, core.DescriptorFor(tc.action).StopsEngine, tc.wantStop)
			}

			var stopped []string
			stop := func(id string) { stopped = append(stopped, id) }
			act := core.Action{Action: tc.action, ID: agentID, Fields: tc.fields}
			if _, err := Dispatch(ctx, act, stop); err != nil {
				t.Fatalf("Dispatch(%s): %v", tc.action, err)
			}

			if gotStop := len(stopped) > 0; gotStop != tc.wantStop {
				t.Errorf("Dispatch(%s) stopped engine = %v, want %v", tc.action, gotStop, tc.wantStop)
			}
			if tc.wantStop && (len(stopped) != 1 || stopped[0] != agentID) {
				t.Errorf("Dispatch(%s) stopEngine calls = %v, want [%s]", tc.action, stopped, agentID)
			}
			tc.check(t, agentID)
		})
	}
}

// TestDispatchCreatesSession checks the CreatesSession/TargetsRoot descriptor
// against the actual dispatch: the create verbs return a NewID, and the verbs
// flagged TargetsRoot return a workgroup root (which a client resolves to its
// first agent) while the others return an agent id directly. A nil stopEngine is
// fine — create verbs never stop an engine.
func TestDispatchCreatesSession(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()
	rootID, err := CreateWorkspace(ctx, "host", nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		action string
		act    core.Action
	}{
		{core.ActionAddAgent, core.Action{Action: core.ActionAddAgent, ID: rootID}},
		{core.ActionNewWorkgroup, core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"name": "wg", "prompt": "go"}}},
		{core.ActionCreateWorkspace, core.Action{Action: core.ActionCreateWorkspace, Fields: map[string]string{"name": "ws2", "defaultAgent": "1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			desc := core.DescriptorFor(tc.action)
			if !desc.CreatesSession {
				t.Fatalf("DescriptorFor(%s).CreatesSession = false, want true", tc.action)
			}
			newID, err := Dispatch(ctx, tc.act, nil)
			if err != nil {
				t.Fatal(err)
			}
			if newID == "" {
				t.Fatalf("Dispatch(%s) returned no NewID", tc.action)
			}
			// TargetsRoot must match whether the created id is actually a root.
			if got := getSession(t, newID).IsRoot(); got != desc.TargetsRoot {
				t.Errorf("%s: created id IsRoot() = %v, but descriptor TargetsRoot = %v",
					tc.action, got, desc.TargetsRoot)
			}
		})
	}
}
