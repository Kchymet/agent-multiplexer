package source

import (
	"fmt"
	"strings"
	"testing"

	"amux/internal/core"
)

// Regression: a coordinator idle between turns while a member runs is
// running, not waiting. Users read "waiting" as "blocked on me".
func TestCoordinatorIdleMemberRunningIsRunning(t *testing.T) {
	for _, own := range []string{core.StateReady, core.StateIdle, core.StateUnknown} {
		v := superviseCoordinator(own, "", "", []memberView{
			member("m1", "a", core.StateRunning), member("m2", "b", core.StateRunning),
		}, watching())
		if v.state != core.StateRunning || v.lead != core.StateRunning || len(v.supervision.NeedsYou) != 0 {
			t.Fatalf("own %s: state/lead/needsYou = %q/%q/%v, want running", own, v.state, v.lead, v.supervision.NeedsYou)
		}
		if got := coordinatorStatus(v, 2, ""); !strings.HasPrefix(got, "running · 2 agents") {
			t.Errorf("own %s: status = %q, want it to lead \"running · 2 agents\"", own, got)
		}
	}
}

// The workgroup headline matrix: waiting only when someone is blocked on the
// user (naming how many), else running when anyone runs, else ready/idle.
func TestWorkgroupRollupMatrix(t *testing.T) {
	cases := []struct {
		own     string
		goal    string
		members []string
		state   string
		status  string // prefix
		needs   []string
	}{
		// Nobody works: the coordinator's own state.
		{own: core.StateReady, members: []string{core.StateReady, core.StateIdle}, state: core.StateReady, status: "ready · 2 agents"},
		{own: core.StateIdle, members: []string{core.StateIdle}, state: core.StateIdle, status: "idle · 1 agent"},
		{own: core.StateReady, state: core.StateReady, status: "ready · 0 agents"},
		// Someone runs, nobody blocked: running.
		{own: core.StateRunning, members: []string{core.StateReady}, state: core.StateRunning, status: "running · 1 agent"},
		{own: core.StateReady, members: []string{core.StateRunning, core.StateReady}, state: core.StateRunning, status: "running · 2 agents"},
		{own: core.StateIdle, members: []string{core.StateRunning}, state: core.StateRunning, status: "running · 1 agent"},
		{own: core.StateRunning, members: []string{core.StateRunning}, state: core.StateRunning, status: "running · 1 agent"},
		{own: core.StateReady, members: []string{core.StateUnknown}, state: core.StateRunning, status: "running · 1 agent"},
		// Someone blocked on the user: waiting, naming how many.
		{own: core.StateReady, members: []string{core.StateWaiting, core.StateRunning}, state: core.StateWaiting,
			status: "waiting · 1 agent needs you · needs you: m0", needs: []string{"m0"}},
		{own: core.StateRunning, members: []string{core.StateWaiting}, state: core.StateWaiting,
			status: "waiting · 1 agent needs you", needs: []string{"m0"}},
		{own: core.StateWaiting, members: []string{core.StateRunning}, state: core.StateWaiting,
			status: "waiting · 1 agent needs you · needs you: coordinator", needs: []string{CoordinatorLabel}},
		{own: core.StateWaiting, members: []string{core.StateWaiting, core.StateWaiting}, state: core.StateWaiting,
			status: "waiting · 3 agents need you", needs: []string{CoordinatorLabel, "m0", "m1"}},
		{own: core.StateReady, goal: "input", members: []string{core.StateReady}, state: core.StateWaiting,
			status: "waiting · 1 agent needs you", needs: []string{CoordinatorLabel}},
		// A goal needing input while the coordinator runs is not yet a prompt.
		{own: core.StateRunning, goal: "input", members: []string{core.StateReady}, state: core.StateRunning, status: "running · 1 agent"},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("own=%s goal=%s members=%v", tc.own, tc.goal, tc.members)
		t.Run(name, func(t *testing.T) {
			var ms []memberView
			for i, st := range tc.members {
				ms = append(ms, member(fmt.Sprintf("id%d", i), fmt.Sprintf("m%d", i), st))
			}
			v := superviseCoordinator(tc.own, tc.goal, "", ms, watching())
			if v.state != tc.state {
				t.Errorf("state = %q, want %q", v.state, tc.state)
			}
			if got := coordinatorStatus(v, len(ms), ""); !strings.HasPrefix(got, tc.status) {
				t.Errorf("status = %q, want prefix %q", got, tc.status)
			}
			if strings.Join(v.supervision.NeedsYou, ",") != strings.Join(tc.needs, ",") {
				t.Errorf("needsYou = %v, want %v", v.supervision.NeedsYou, tc.needs)
			}
			if (v.state == core.StateWaiting) != (len(v.supervision.NeedsYou) > 0) {
				t.Errorf("state %q disagrees with needsYou %v", v.state, v.supervision.NeedsYou)
			}
		})
	}
}
