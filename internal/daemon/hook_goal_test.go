package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/store"
)

// The host `goal` control on a Claude coordinator moves its session-local goal
// file: pause stops continuation, resume reports a resume (so an idle
// coordinator is nudged), and Codex-only fields are refused, not dropped.
func TestSteerHookGoal(t *testing.T) {
	dir := t.TempDir()
	sess := store.Session{ID: "wg1", Agent: "claude", Dir: dir}
	read := func() claudegoal.State {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, claudegoal.StateRel))
		if err != nil {
			t.Fatal(err)
		}
		s, err := claudegoal.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	field := func(kv ...string) map[string]string {
		m := map[string]string{core.SteerVerb: core.SteerGoal}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}

	if _, err := steerHookGoal(sess, field(core.SteerGoalStatus, "paused")); err == nil {
		t.Fatal("paused a goal that does not exist")
	}
	resume, err := steerHookGoal(sess, field(core.SteerGoalObjective, "ship the fix"))
	if err != nil || !resume {
		t.Fatalf("set objective: resume=%v err=%v", resume, err)
	}
	if s := read(); s.Status != claudegoal.StatusActive || s.Objective != "ship the fix" {
		t.Fatalf("after objective: %+v", s)
	}
	if resume, err := steerHookGoal(sess, field(core.SteerGoalStatus, "paused")); err != nil || resume {
		t.Fatalf("pause: resume=%v err=%v", resume, err)
	}
	if s := read(); s.Status != claudegoal.StatusPaused {
		t.Fatalf("after pause: %+v", s)
	}
	if resume, err := steerHookGoal(sess, field(core.SteerGoalStatus, "active")); err != nil || !resume {
		t.Fatalf("resume: resume=%v err=%v", resume, err)
	}
	if _, err := steerHookGoal(sess, field(core.SteerGoalBudget, "1000")); err == nil {
		t.Fatal("token budget accepted on a claude coordinator")
	}
	if _, err := steerHookGoal(sess, field(core.SteerGoalStatus, "clear")); err != nil {
		t.Fatal(err)
	}
	if s := read(); !s.None() {
		t.Fatalf("after clear: %+v", s)
	}
}
