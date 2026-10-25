package wsops

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/store"
)

// A coordinator's guide must describe the lifecycle it actually has, and teach
// only the operations its mailbox grant authorizes: a guide that promises goal
// mode on a harness without one, or advertises a host-only operation, sends the
// model into failures the user then has to unpick.
func TestCoordinatorGuideDescribesItsRealLifecycle(t *testing.T) {
	root := store.Session{ID: "wg1", Name: "payments", Agent: agent.GoalRuntime, Dir: "/sandbox/wg1"}
	if !agent.NativeGoals(root) {
		t.Fatal("fixture is not a goal coordinator")
	}
	guide := coordinatorGuide(root)
	for _, want := range []string{
		"native", "update_goal", "complete",
		"request_user_input",             // ask only for genuine input
		"amux do steer wg1 -f verb=goal", // the user's pause/resume/clear
		"amux do add-agent wg1",
		"amux do set-archived",
		"choose the sensible default", // decide and keep going
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("goal coordinator guide missing %q", want)
		}
	}
	// Restoring an archived member is the user's operation, not the
	// coordinator's: its grant refuses it. The workgroup archive command is named
	// only as the user's host operation, never as a step the coordinator takes.
	if strings.Contains(guide, "amux do unarchive") {
		t.Error("coordinator guide advertises the host-only amux do unarchive")
	}
	assertCompletionLeavesWorkgroupOpen(t, "goal coordinator", guide, "wg1")

	// An explicitly non-goal coordinator is told the truth instead.
	plain := store.Session{ID: "wg2", Name: "payments", Agent: "claude", Dir: "/sandbox/wg2"}
	if agent.NativeGoals(plain) {
		t.Fatal("fixture unexpectedly has native goals")
	}
	other := coordinatorGuide(plain)
	if !strings.Contains(other, "no native goal engine") {
		t.Error("non-goal coordinator guide claims or omits its lifecycle")
	}
	if strings.Contains(other, "update_goal") {
		t.Error("non-goal coordinator guide teaches a tool it does not have")
	}
	if !strings.Contains(other, agent.GoalRuntime) {
		t.Error("non-goal coordinator guide does not say which runtime does run goals")
	}
	assertCompletionLeavesWorkgroupOpen(t, "non-goal coordinator", other, "wg2")
}

// Finishing a task must never read as "the workgroup gets archived": on every
// harness the guide tells the coordinator the workgroup stays open, to ask the
// user whether to archive it or continue, and that archiving is the user's host
// operation — not something the coordinator (or a member on its behalf) does.
func assertCompletionLeavesWorkgroupOpen(t *testing.T, kind, guide, id string) {
	t.Helper()
	// The guide is hard-wrapped prose; match phrases against its unwrapped form.
	flat := strings.Join(strings.Fields(guide), " ")
	for _, want := range []string{
		"archives nothing",
		"ask the user whether to archive the workgroup",
		"amux workgroup archive " + id,
		"Never archive this workgroup yourself — not with `amux agent done`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("%s guide missing %q", kind, want)
		}
	}
	for _, forbidden := range []string{
		"automatically marks the workgroup done",
		"marks the workgroup done",
		"stops its coordinator and members",
	} {
		if strings.Contains(flat, forbidden) {
			t.Errorf("%s guide still promises auto-archive: %q", kind, forbidden)
		}
	}
}

// The member guide is unchanged by goal mode: an ordinary agent is dispatched by
// its coordinator and has no goal of its own to reason about.
func TestMemberGuideMentionsNoGoals(t *testing.T) {
	guide := memberGuide(store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Branch: "amux/wg1-m1"})
	for _, forbidden := range []string{"update_goal", "goal mode", "thread goal"} {
		if strings.Contains(guide, forbidden) {
			t.Errorf("member guide mentions %q", forbidden)
		}
	}
}

// The console dispatches creations for the user, so its guide must name the
// creation fields the daemon actually accepts — a guide that teaches a field
// the allowlist refuses sends the model into an error it cannot diagnose.
func TestConsoleGuideTeachesCoordinatorFields(t *testing.T) {
	guide := consoleGuide(store.Session{ID: "console", Agent: "claude", Dir: "/sandbox/console", Mode: store.ModeConsole})
	for _, want := range []string{FieldCoordinator, FieldCoordinatorModel, agent.GoalRuntime} {
		if !strings.Contains(guide, want) {
			t.Errorf("console guide does not teach %q", want)
		}
	}
}
