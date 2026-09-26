package codexapp

import (
	"errors"
	"testing"
)

func TestWorkgroupCompletionSurvivesRestart(t *testing.T) {
	s, _ := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Status: GoalActive, Objective: "task"})
	notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalComplete, Objective: "task"})
	w := s.RestartWork()
	if w.GoalKey != "" || w.CompletedGoalKey == "" {
		t.Fatalf("pending completion lost: %+v", w)
	}
	for _, tc := range []struct {
		name string
		work *RestartWork
		want bool
	}{
		{"pending archive", &w, true},
		{"finished during shutdown", &RestartWork{ThreadID: w.ThreadID, GoalKey: w.CompletedGoalKey}, true},
		{"host restore", nil, false},
		{"different goal", &RestartWork{ThreadID: w.ThreadID, CompletedGoalKey: "other"}, false},
		{"different thread", &RestartWork{ThreadID: "other", CompletedGoalKey: w.CompletedGoalKey}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, goal: &threadGoal{ThreadID: w.ThreadID, Status: GoalComplete, Objective: "task"}}
			go fs.loop()
			defer fs.close()
			restored := New(Config{Goals: true, ResumeThreadID: w.ThreadID, RestartWork: tc.work})
			defer restored.Close()
			attach(t, restored, client)
			done, err := restored.CompleteWorkgroup(func() error { return nil })
			if err != nil || done != tc.want {
				t.Fatalf("done=%t want=%t err=%v", done, tc.want, err)
			}
		})
	}
}

func TestWorkgroupCompletionWaitsForFinalTurn(t *testing.T) {
	s, _ := goalSession(t, true)
	goal := &threadGoal{ThreadID: s.ThreadID(), Objective: "ship", Status: GoalActive, CreatedAt: 1}
	notifyGoal(s, goal)
	peerTurn(s, "final")
	goal.Status = GoalComplete
	notifyGoal(s, goal)
	commits := 0
	commit := func() error { commits++; return nil }
	if done, err := s.CompleteWorkgroup(commit); done || err != nil || commits != 0 {
		t.Fatalf("archived during final response: done=%t err=%v commits=%d", done, err, commits)
	}
	endTurn(s, "final", "completed")
	if done, err := s.CompleteWorkgroup(commit); !done || err != nil || commits != 1 {
		t.Fatalf("did not archive after final turn: done=%t err=%v commits=%d", done, err, commits)
	}
	if done, _ := s.CompleteWorkgroup(commit); done || commits != 1 {
		t.Fatal("completion committed twice")
	}
}

func TestWorkgroupCompletionRequiresNewCompletedGoal(t *testing.T) {
	for _, status := range []string{GoalActive, GoalPaused, GoalBlocked, GoalBudgetLimited, GoalUsageLimited, GoalComplete} {
		t.Run(status, func(t *testing.T) {
			s, _ := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Status: GoalActive, Objective: "task"})
			notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: status, Objective: "task"})
			called := false
			done, err := s.CompleteWorkgroup(func() error { called = true; return nil })
			if err != nil || done != (status == GoalComplete) || called != done {
				t.Fatalf("done=%t called=%t err=%v", done, called, err)
			}
		})
	}
	for _, goals := range []bool{false, true} {
		s, _ := goalSessionWith(t, goals, &threadGoal{ThreadID: "thr_1", Status: GoalComplete, Objective: "previous task"})
		if done, _ := s.CompleteWorkgroup(func() error { t.Fatal("restored completed goal re-archived"); return nil }); done {
			t.Fatal("restored completed goal archived")
		}
	}
	s, _ := goalSession(t, false)
	notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalActive})
	notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalComplete})
	s.CompleteWorkgroup(func() error { t.Fatal("ordinary session archived"); return nil })
}

func TestWorkgroupCompletionRechecksStateAndRetriesStoreFailures(t *testing.T) {
	for _, change := range []string{"new turn", "new goal", "cleared", "pending task", "pending prompt", "goal operation", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			s, _ := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Status: GoalActive})
			notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalComplete})
			switch change {
			case "new turn":
				peerTurn(s, "new")
			case "new goal":
				notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalActive, Objective: "new"})
			case "cleared":
				notifyGoal(s, nil)
			case "pending task":
				s.goalOpen = map[string]int{"new": 1}
			case "pending prompt":
				s.turnDone = make(chan *turnResult, 1)
			case "goal operation":
				s.goalOp.Lock()
				defer s.goalOp.Unlock()
			case "shutdown":
				s.interruptTransport()
			}
			if done, err := s.CompleteWorkgroup(func() error { t.Fatal("stale completion committed"); return nil }); done || err != nil {
				t.Fatalf("done=%t err=%v", done, err)
			}
		})
	}
	s, _ := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Status: GoalActive})
	notifyGoal(s, &threadGoal{ThreadID: s.ThreadID(), Status: GoalComplete})
	want := errors.New("store unavailable")
	if done, err := s.CompleteWorkgroup(func() error { return want }); done || !errors.Is(err, want) {
		t.Fatalf("failed commit: done=%t err=%v", done, err)
	}
	if done, err := s.CompleteWorkgroup(func() error { return nil }); !done || err != nil {
		t.Fatalf("retry: done=%t err=%v", done, err)
	}
}
