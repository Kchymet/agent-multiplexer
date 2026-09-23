package daemon

import (
	"testing"

	"amux/internal/codexapp"
	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A goal control sent over the wire is copied through the provider verbatim and
// handed to codexapp.SetGoal as a string. Nothing in that path converts a
// vocabulary, so a wire status that the runtime does not accept would be a
// round-trip that only ever fails — and one the runtime accepts but the wire
// rejects would be a control a remote user simply cannot reach. This pins the
// two vocabularies to each other; it asserts spellings, and changes nothing.
func TestWireGoalStatusesMatchTheRuntime(t *testing.T) {
	for wire, runtime := range map[string]string{
		harnessproto.GoalActive:   codexapp.GoalActive,
		harnessproto.GoalPaused:   codexapp.GoalPaused,
		harnessproto.GoalComplete: codexapp.GoalComplete,
		harnessproto.GoalClear:    codexapp.GoalClear,
	} {
		if wire != runtime {
			t.Errorf("wire status %q != runtime status %q", wire, runtime)
		}
		if !harnessproto.GoalStatuses[wire] {
			t.Errorf("status %q is missing from GoalStatuses", wire)
		}
	}
	if len(harnessproto.GoalStatuses) != 4 {
		t.Errorf("GoalStatuses = %v, want exactly the four controls a user may set", harnessproto.GoalStatuses)
	}
	// The statuses a runtime reports but no caller may set must stay out of the
	// control vocabulary, or a consumer would offer them as buttons.
	for _, observed := range []string{codexapp.GoalBlocked, codexapp.GoalUsageLimited, codexapp.GoalBudgetLimited} {
		if harnessproto.GoalStatuses[observed] {
			t.Errorf("runtime-only status %q is offered as a user control", observed)
		}
	}
	if harnessproto.VerbGoal != core.SteerGoal {
		t.Errorf("wire verb %q != daemon verb %q", harnessproto.VerbGoal, core.SteerGoal)
	}
}
