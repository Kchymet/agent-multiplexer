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
	// Restoring an archived member and archiving the workgroup itself are the
	// user's operations, not the coordinator's: its grant refuses them.
	for _, forbidden := range []string{"amux do unarchive", "amux workgroup archive"} {
		if strings.Contains(guide, forbidden) {
			t.Errorf("coordinator guide advertises the host-only %q", forbidden)
		}
	}

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
