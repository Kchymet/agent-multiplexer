package wsops

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/store"
)

// Every coordinator guide, whatever its goal runtime, makes archiving its own
// finished members an expected step, while self/workgroup archive still waits
// for the user.
func TestCoordinatorGuideExpectsArchivingFinishedMembers(t *testing.T) {
	for _, root := range []store.Session{
		{ID: "wg1", Name: "codex", Agent: agent.GoalRuntime, Dir: "/sandbox/wg1"},
		{ID: "wg2", Name: "claude", Agent: agent.HookGoalRuntime, Dir: "/sandbox/wg2"},
		{ID: "wg3", Name: "plain", Agent: "hermes", Dir: "/sandbox/wg3"},
	} {
		flat := strings.Join(strings.Fields(coordinatorGuide(root)), " ")
		for _, want := range []string{
			"## Archiving finished members",
			"This is an expected step, not an option",
			"A member is **finished** when its PR is merged or closed (see \"Track every member PR to merged or closed\"), or its task was delivered with no PR, and you will not give it more work.",
			"A member that still owns an open PR is not finished: never archive it.",
			"Archive it then, during the task",
			"amux do set-archived <member> -f archived=true",
			"never archive a member while it is running",
			"Before you report a task complete (and before marking a goal complete), archive every finished member",
			"Keep a finished member only if you expect to reuse it",
			"with the reason",
			"(`amux agent done`) still needs explicit user confirmation",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("%s coordinator guide lacks %q", root.Agent, want)
			}
		}
		if strings.Contains(flat, "You may still mark verified members done") {
			t.Errorf("%s coordinator guide still frames member archive as optional", root.Agent)
		}
	}
	codex := strings.Join(strings.Fields(coordinatorGuide(store.Session{ID: "wg1", Agent: agent.GoalRuntime})), " ")
	if !strings.Contains(codex, "Archive every finished member first") {
		t.Error("native goal section does not tie goal completion to member archive")
	}
	claude := strings.Join(strings.Fields(coordinatorGuide(store.Session{ID: "wg2", Agent: agent.HookGoalRuntime})), " ")
	if !strings.Contains(claude, "it lists them with the archive command") {
		t.Error("hook goal section does not mention the goal complete reminder")
	}
	member := memberGuide(store.Session{ID: "a1", RootID: "wg1", Agent: "claude", Dir: "/sandbox/a1", Branch: "amux/wg1-a1"})
	if strings.Contains(member, "Archiving finished members") {
		t.Error("member guide carries the coordinator's archive duty")
	}
}
