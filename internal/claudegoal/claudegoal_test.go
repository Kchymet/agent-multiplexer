package claudegoal

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

// Every user task becomes the active goal, replacing whatever was there.
func TestPromptEstablishesGoal(t *testing.T) {
	s := OnPrompt(State{}, "  ship the billing fix  ", t0)
	if s.Status != StatusActive || s.Objective != "ship the billing fix" || s.Created != t0.Unix() {
		t.Fatalf("new task: %+v", s)
	}
	for _, prev := range []string{StatusComplete, StatusBlocked, StatusPaused, StatusBudgetLimited, StatusActive} {
		got := OnPrompt(State{Objective: "old", Status: prev, Continuations: 7}, "new task", t0)
		if got.Objective != "new task" || got.Status != StatusActive || got.Continuations != 0 {
			t.Errorf("task after %s: %+v", prev, got)
		}
	}
	if got := OnPrompt(State{Objective: "keep", Status: StatusActive}, "   ", t0); got.Objective != "keep" {
		t.Errorf("blank prompt replaced the goal: %+v", got)
	}
}

// A reply to the coordinator's question is input to the goal, not a new task;
// a resume nudge continues the goal it names and establishes nothing alone.
func TestReplyAndResumeKeepTheObjective(t *testing.T) {
	asked := State{Objective: "migrate the db", Status: StatusInput, Note: "which region?", Continuations: 3}
	got := OnPrompt(asked, "eu-west-1", t0)
	if got.Status != StatusActive || got.Note != "" || got.Continuations != 0 ||
		!strings.HasPrefix(got.Objective, "migrate the db") || !strings.Contains(got.Objective, "User reply: eu-west-1") {
		t.Fatalf("reply: %+v", got)
	}
	paused := State{Objective: "migrate the db", Status: StatusPaused, Continuations: 9}
	got = OnPrompt(paused, ResumePrompt, t0)
	if got.Status != StatusActive || got.Objective != "migrate the db" || got.Continuations != 0 {
		t.Fatalf("resume: %+v", got)
	}
	if got := OnPrompt(State{}, ResumePrompt, t0); !got.None() {
		t.Fatalf("resume nudge with no goal established one: %+v", got)
	}
}

// The Stop hook continues only an active goal, and only up to the budget.
func TestStopContinuesActiveGoalWithinBudget(t *testing.T) {
	s := State{Objective: "ship it", Status: StatusActive}
	next, reason, cont := OnStop(s, t0)
	if !cont || next.Continuations != 1 || !strings.Contains(reason, "Objective: ship it") ||
		!strings.Contains(reason, "amux agent goal complete") || !strings.Contains(reason, "amux agent goal ask") {
		t.Fatalf("active stop: cont=%v next=%+v reason=%q", cont, next, reason)
	}
	for _, status := range []string{"", StatusPaused, StatusInput, StatusBlocked, StatusComplete, StatusBudgetLimited} {
		if _, _, cont := OnStop(State{Objective: "x", Status: status}, t0); cont {
			t.Errorf("stop continued a %q goal", status)
		}
	}
	spent := State{Objective: "x", Status: StatusActive, Continuations: MaxContinuations}
	next, _, cont = OnStop(spent, t0)
	if cont || next.Status != StatusBudgetLimited {
		t.Fatalf("budget: cont=%v next=%+v", cont, next)
	}
}

func TestModelTransitions(t *testing.T) {
	if _, err := Mark(State{}, StatusComplete, "", t0); err == nil {
		t.Error("marked a goal that does not exist")
	}
	active := State{Objective: "x", Status: StatusActive}
	for _, status := range []string{StatusComplete, StatusInput, StatusBlocked} {
		got, err := Mark(active, status, "why", t0)
		if err != nil || got.Status != status || got.Note != "why" {
			t.Errorf("mark %s: %+v %v", status, got, err)
		}
	}
	if _, err := Mark(active, StatusPaused, "", t0); err == nil {
		t.Error("the model paused its own goal")
	}
}

func TestUserControls(t *testing.T) {
	active := State{Objective: "x", Status: StatusActive, Continuations: 4}
	paused, resume, err := Control(active, ControlPaused, "", t0)
	if err != nil || paused.Status != StatusPaused || resume {
		t.Fatalf("pause: %+v %v %v", paused, resume, err)
	}
	resumed, resume, err := Control(paused, ControlActive, "", t0)
	if err != nil || resumed.Status != StatusActive || !resume || resumed.Continuations != 0 {
		t.Fatalf("resume: %+v %v %v", resumed, resume, err)
	}
	if _, resume, _ := Control(resumed, ControlActive, "", t0); resume {
		t.Error("re-activating an active goal reported a resume")
	}
	cleared, _, err := Control(active, ControlClear, "", t0)
	if err != nil || !cleared.None() {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	set, resume, err := Control(State{}, "", "new objective", t0)
	if err != nil || set.Objective != "new objective" || set.Status != StatusActive || !resume {
		t.Fatalf("objective on empty: %+v %v %v", set, resume, err)
	}
	for _, bad := range []struct{ status, objective string }{
		{"", ""}, {"bogus", ""}, {ControlClear, "x"},
	} {
		if _, _, err := Control(active, bad.status, bad.objective, t0); err == nil {
			t.Errorf("control %+v accepted", bad)
		}
	}
	if _, _, err := Control(State{}, ControlPaused, "", t0); err == nil {
		t.Error("paused a goal that does not exist")
	}
}

func TestStateRoundTripAndTruncation(t *testing.T) {
	s := OnPrompt(State{}, strings.Repeat("é", MaxObjectiveBytes), t0)
	if len(s.Objective) > MaxObjectiveBytes+len("…") || !strings.HasSuffix(s.Objective, "…") {
		t.Fatalf("objective not truncated: %d bytes", len(s.Objective))
	}
	got, err := Decode(Encode(s))
	if err != nil || got != s {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if got, err := Decode(nil); err != nil || !got.None() {
		t.Fatalf("empty file: %+v %v", got, err)
	}
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("corrupt state decoded")
	}
}

// A waiting goal lets exactly one Stop through, then is active again: the turn
// the coordinator's armed wake-up starts ends under goal pressure. The wait
// neither spends nor refunds continuation budget.
func TestWaitLetsOneStopThrough(t *testing.T) {
	active := State{Objective: "land the PR", Status: StatusActive, Continuations: 5}
	waiting, err := Mark(active, StatusWaiting, "member m1's CI", t0)
	if err != nil || waiting.Status != StatusWaiting {
		t.Fatalf("mark wait: %+v %v", waiting, err)
	}
	if !strings.Contains(Summary(waiting), "waiting") || !strings.Contains(Summary(waiting), "member m1's CI") {
		t.Errorf("summary = %q", Summary(waiting))
	}
	woken, _, cont := OnStop(waiting, t0)
	if cont || woken.Status != StatusActive || woken.Continuations != 5 {
		t.Fatalf("stop while waiting: cont=%v %+v", cont, woken)
	}
	next, reason, cont := OnStop(woken, t0)
	if !cont || next.Continuations != 6 || reason == "" {
		t.Fatalf("stop after wake-up turn: cont=%v %+v", cont, next)
	}
	// Waiting is on members, not the user: a user prompt is a new task.
	if got := OnPrompt(waiting, "new task", t0); got.Objective != "new task" || got.Status != StatusActive {
		t.Fatalf("prompt while waiting: %+v", got)
	}
	// The continuation prompt teaches the idiomatic wait, not busy-polling.
	for _, want := range []string{"amux agent goal wait", "run_in_background", "Monitor", "never busy-poll"} {
		if !strings.Contains(ContinuationPrompt(active), want) {
			t.Errorf("continuation prompt missing %q", want)
		}
	}
}
