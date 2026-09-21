package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRestartResumesOnlyPreviouslyActiveGoal(t *testing.T) {
	saved := &threadGoal{ThreadID: "thr_saved", Objective: "keep working", Status: "active", CreatedAt: 123}
	for _, tc := range []struct {
		name, status      string
		work              *RestartWork
		wantSet, wantTurn bool
	}{
		{"running", "active", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"paused during restart", "paused", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, true, false},
		{"deliberately paused", "paused", &RestartWork{ThreadID: saved.ThreadID}, false, false},
		{"paused with busy ordinary turn", "paused", &RestartWork{ThreadID: saved.ThreadID, Continue: true}, false, false},
		{"blocked", "blocked", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"complete", "complete", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"usage limited", "usageLimited", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"budget limited", "budgetLimited", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"different thread", "paused", &RestartWork{ThreadID: "other", GoalKey: saved.key()}, false, false},
		{"different goal", "paused", &RestartWork{ThreadID: saved.ThreadID, GoalKey: "different"}, false, false},
		{"ordinary busy turn", "", &RestartWork{ThreadID: saved.ThreadID, Continue: true}, false, true},
		{"cleared goal", "", &RestartWork{ThreadID: saved.ThreadID, GoalKey: saved.key()}, false, false},
		{"normal attach", "paused", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
			if tc.status != "" {
				g := *saved
				g.Status = tc.status
				fs.goal = &g
			}
			go fs.loop()
			defer fs.close()
			s := New(Config{SessionID: "goal", ResumeThreadID: saved.ThreadID, RestartWork: tc.work})
			defer s.Close()
			attach(t, s, client)
			call, set := fs.sawCall("thread/goal/set")
			_, turn := fs.sawCall("turn/start")
			if set != tc.wantSet || turn != tc.wantTurn {
				t.Fatalf("set=%t turn=%t", set, turn)
			}
			if set {
				var p map[string]any
				json.Unmarshal(call.Params, &p)
				if len(p) != 2 || p["threadId"] != saved.ThreadID || p["status"] != "active" {
					t.Fatalf("reset goal accounting: %s", call.Params)
				}
				if s.RestartWork().GoalKey != saved.key() {
					t.Fatal("resumed goal lost live tracking")
				}
			}
		})
	}
}

func TestRestartGoalObservationHonorsPauseAndClear(t *testing.T) {
	s, fs, client := newFakePair(t)
	defer fs.close()
	defer s.Close()
	attach(t, s, client)
	goal := threadGoal{ThreadID: s.ThreadID(), Objective: "task", Status: "active", CreatedAt: 1}
	observe := func(id string, g *threadGoal) {
		b, _ := json.Marshal(map[string]any{"threadId": id, "goal": g})
		s.onNotify("thread/goal/updated", b)
	}
	observe(s.ThreadID(), &goal)
	if s.RestartWork().GoalKey == "" {
		t.Fatal("active goal not captured")
	}
	goal.Status = "paused"
	observe("other", &goal)
	if s.RestartWork().GoalKey == "" {
		t.Fatal("foreign pause changed goal")
	}
	observe(s.ThreadID(), &goal)
	if w := s.RestartWork(); w.GoalKey != "" || w.Continue {
		t.Fatal("explicit pause would restart")
	}
	observe(s.ThreadID(), nil)
	if s.RestartWork().GoalKey != "" {
		t.Fatal("clear retained goal")
	}
	goal.Status = "active"
	observe(s.ThreadID(), &goal)
	s.interruptTransport()
	goal.Status = "paused"
	observe(s.ThreadID(), &goal)
	if s.RestartWork().GoalKey == "" {
		t.Fatal("shutdown overwrote pre-restart intent")
	}
}

func TestRestartSnapshotSurvivesTransportDrain(t *testing.T) {
	s, fs, client := newFakePair(t)
	defer fs.close()
	defer s.Close()
	attach(t, s, client)
	s.onNotify("turn/started", json.RawMessage(`{"threadId":"thr_1","turn":{"id":"busy"}}`))
	m := &Manager{sup: map[string]*Supervisor{"headless": s}}
	if !m.RestartSnapshot()["headless"].Continue {
		t.Fatal("headless running turn missing from restart snapshot")
	}
	s.interruptTransport()
	s.failTurn()           // transport cleanup discards the active turn
	s.interruptTransport() // daemon cancellation and teardown can both call this
	if !m.RestartSnapshot()["headless"].Continue {
		t.Fatal("transport drain lost running work")
	}
	s.Close()
	if len(m.RestartSnapshot()) != 0 {
		t.Fatal("closed supervisor was marked for restart")
	}
}

func TestGoalReadFailureDoesNotContinueBlindly(t *testing.T) {
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, failMethod: "thread/goal/get"}
	go fs.loop()
	defer fs.close()
	s := New(Config{ResumeThreadID: "thr_saved", RestartWork: &RestartWork{ThreadID: "thr_saved", Continue: true}})
	defer s.Close()
	if err := s.attach(context.Background(), client); err == nil || !strings.Contains(err.Error(), "read goal") {
		t.Fatalf("expected goal read error, got %v", err)
	}
	if _, ok := fs.sawCall("turn/start"); ok {
		t.Fatal("submitted work without checking goal status")
	}
}
