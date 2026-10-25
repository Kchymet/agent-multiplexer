package codexapp

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// Completing a goal ends the task, not the workgroup: the completed goal stays
// observable on the thread after the final turn, nothing about the supervisor
// asks the host to archive anything, and the user's next task on the same open
// workgroup replaces the finished goal with a fresh one.
func TestCompletedGoalStaysObservableAndNextTaskStartsFreshGoal(t *testing.T) {
	s, fs := goalSession(t, true)
	goal := &threadGoal{ThreadID: s.ThreadID(), Objective: "ship the fix", Status: GoalActive, CreatedAt: 7}
	peerTurn(s, "final")
	fs.setGoalState(goal)
	notifyGoal(s, goal)
	done := *goal
	done.Status = GoalComplete
	done.TokensUsed = 4242
	fs.setGoalState(&done)
	notifyGoal(s, &done)
	endTurn(s, "final", "completed")

	got, ok := s.Goal()
	if !ok || got.Status != GoalComplete || got.Objective != "ship the fix" || got.TokensUsed != 4242 {
		t.Fatalf("completed goal not observable after the final turn: %+v ok=%t", got, ok)
	}
	// A completed goal is not interrupted work: restart evidence names no goal
	// and asks for no continuation, so nothing survives a restart to act on it.
	if w := s.RestartWork(); w.GoalKey != "" || w.Continue {
		t.Fatalf("restart work after completion = %+v, want none", w)
	}
	if g := fs.goalState(); g == nil || g.Status != GoalComplete {
		t.Fatalf("completion changed the native goal: %+v", g)
	}

	// The workgroup stays open: the next user task replaces the finished goal.
	peerTurn(s, "next")
	userMessage(s, "next", "item_next", "now add the regression test")
	if !fs.awaitCall("thread/goal/set", 2*time.Second) {
		t.Fatal("next task did not establish a goal")
	}
	fresh := awaitGoal(t, fs, func(g *threadGoal) bool { return g != nil && g.Status == GoalActive }, 2*time.Second)
	if fresh == nil || fresh.Objective != "now add the regression test" || fresh.Status != GoalActive {
		t.Fatalf("next goal = %+v, want a fresh active goal for the new task", fresh)
	}
	if fresh.TokensUsed != 0 || fresh.CreatedAt == 7 {
		t.Fatalf("fresh goal inherited the completed goal's accounting: %+v", fresh)
	}
	if got, ok := s.Goal(); !ok || got.Status != GoalActive || got.Objective != "now add the regression test" {
		t.Fatalf("supervisor view after the next task = %+v ok=%t", got, ok)
	}
}

// A thread whose goal is already complete at resume is settled, whatever the
// restart journal says about it: evidence written before completion stopped
// being restart work (a legacy completedGoalKey), evidence that the goal was
// active at shutdown and finished before resume, or no evidence at all. None of
// them reactivates the goal, hides it, or turns it back into restart work.
func TestResumedCompletedGoalIsSettledWhateverTheRestartEvidence(t *testing.T) {
	finished := &threadGoal{ThreadID: "thr_1", Status: GoalComplete, Objective: "task", CreatedAt: 3}
	var legacy RestartWork
	if err := json.Unmarshal([]byte(fmt.Sprintf(`{"threadId":"thr_1","completedGoalKey":%q}`, finished.key())), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ThreadID != "thr_1" || legacy.GoalKey != "" || legacy.Continue {
		t.Fatalf("legacy journal decoded as %+v", legacy)
	}
	for _, tc := range []struct {
		name string
		work *RestartWork
	}{
		{"legacy completed-goal evidence", &legacy},
		{"goal active at shutdown, finished before resume", &RestartWork{ThreadID: "thr_1", GoalKey: finished.key()}},
		{"no restart evidence", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, goal: finished}
			go fs.loop()
			defer fs.close()
			restored := New(Config{Goals: true, ResumeThreadID: "thr_1", RestartWork: tc.work})
			defer restored.Close()
			attach(t, restored, client)
			if _, set := fs.sawCall("thread/goal/set"); set {
				t.Fatal("resume reactivated a completed goal")
			}
			if got, ok := restored.Goal(); !ok || got.Status != GoalComplete || got.Objective != "task" {
				t.Fatalf("completed goal not observable after resume: %+v ok=%t", got, ok)
			}
			if w := restored.RestartWork(); w.GoalKey != "" || w.Continue {
				t.Fatalf("completed goal produced restart work: %+v", w)
			}
		})
	}
}
