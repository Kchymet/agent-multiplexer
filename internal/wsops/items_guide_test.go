package wsops

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/store"
)

// Members are told to emit upward signals as structured items at the end of a
// turn or of their work, not as prose in a final message.
func TestMemberGuideRaisesItems(t *testing.T) {
	flat := strings.Join(strings.Fields(memberGuide(store.Session{ID: "m1", RootID: "wg1", Branch: "amux/wg1-m1", Repo: "api"})), " ")
	for _, want := range []string{
		"## Raise follow-ups as items",
		"amux agent raise --kind follow_up|outstanding|decision_needed|risk|fyi",
		"do not bury it in prose",
		"A workgroup member's item goes to its coordinator, anyone else's to the global inbox",
		"updates that item instead of adding a duplicate",
		"amux agent items done <id>",
		"Name the item ids in your final message",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("member guide lacks %q", want)
		}
	}
}

// Every coordinator guide makes it the triage point: resolve, escalate to the
// global inbox, raise its own end-of-task items, and never archive with an
// untriaged inbox without saying the items move to the global inbox.
func TestCoordinatorGuideTriagesItems(t *testing.T) {
	for _, root := range []store.Session{
		{ID: "wg1", Name: "codex", Agent: agent.GoalRuntime, Dir: "/sandbox/wg1"},
		{ID: "wg2", Name: "claude", Agent: agent.HookGoalRuntime, Dir: "/sandbox/wg2"},
	} {
		flat := strings.Join(strings.Fields(coordinatorGuide(root)), " ")
		for _, want := range []string{
			"## Triage upward items",
			"amux agent items show <id>",
			"amux agent escalate <id> --note \"<why>\"",
			"Do not escalate what you can resolve",
			"goes up as items from you (yours go to the global inbox)",
			"Archiving this workgroup carries every item still open or acknowledged in your inbox to the global inbox",
			"No open item in your inbox: `amux agent items --inbox` lists none",
			"any item still open or acknowledged in your inbox — name each in the question, and say that archiving hands it to the global inbox",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("%s coordinator guide lacks %q", root.Agent, want)
			}
		}
	}
}
