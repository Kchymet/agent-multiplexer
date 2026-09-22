package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// goalSession attaches a supervisor configured as a goal session (a workgroup
// coordinator on the goal runtime) to a fake App Server.
func goalSession(t *testing.T, goals bool) (*Supervisor, *fakeServer) {
	t.Helper()
	return goalSessionWith(t, goals, nil)
}

// goalSessionWith seeds a goal the thread already has BEFORE the handshake, the
// way a live session learns one: the resume read observes it, so the session's
// view and the server agree from the first turn.
func goalSessionWith(t *testing.T, goals bool, initial *threadGoal) (*Supervisor, *fakeServer) {
	t.Helper()
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, goal: initial}
	go fs.loop()
	t.Cleanup(fs.close)
	s := New(Config{SessionID: "coordinator", Goals: goals})
	t.Cleanup(func() { _ = s.Close() })
	attach(t, s, client)
	return s, fs
}

// userMessage delivers the canonical userMessage item the App Server broadcasts
// for any client's input — a daemon prompt, the creation prompt, or a native
// TUI turn started behind amux's back.
func userMessage(s *Supervisor, turnID, itemID, text string) {
	params, _ := json.Marshal(map[string]any{
		"threadId": s.ThreadID(), "turnId": turnID,
		"item": map[string]any{"id": itemID, "type": "userMessage",
			"content": []map[string]any{{"type": "text", "text": text}}},
	})
	s.onNotify("item/started", params)
}

// peerTurn starts a turn the way a native TUI does: the supervisor observes
// turn/started without having issued turn/start itself.
func peerTurn(s *Supervisor, turnID string) {
	params, _ := json.Marshal(map[string]any{"threadId": s.ThreadID(), "turn": map[string]any{"id": turnID}})
	s.onNotify("turn/started", params)
}

func endTurn(s *Supervisor, turnID, status string) {
	params, _ := json.Marshal(map[string]any{"threadId": s.ThreadID(), "turn": map[string]any{"id": turnID, "status": status}})
	s.onNotify("turn/completed", params)
}

func awaitGoal(t *testing.T, fs *fakeServer, want func(*threadGoal) bool, d time.Duration) *threadGoal {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if g := fs.goalState(); want(g) {
			return g
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fs.goalState()
}

// settle gives the asynchronous observer time to act, so "nothing happened" is
// an observation rather than a race the assertion won.
func settle() { time.Sleep(150 * time.Millisecond) }

// A workgroup created empty and then given its first task establishes that task
// as the goal — in the turn the task already started, with no extra kickoff
// turn, whichever client submitted it.
func TestFirstTaskBecomesGoal(t *testing.T) {
	for _, origin := range []string{"daemon prompt", "native TUI"} {
		t.Run(origin, func(t *testing.T) {
			s, fs := goalSession(t, true)
			peerTurn(s, "turn_a")
			userMessage(s, "turn_a", "item_1", "Ship the goal-default feature")
			g := awaitGoal(t, fs, func(g *threadGoal) bool { return g != nil }, 2*time.Second)
			if g == nil || g.Objective != "Ship the goal-default feature" || g.Status != GoalActive {
				t.Fatalf("first task did not become the active goal: %+v", g)
			}
			if _, started := fs.sawCall("turn/start"); started {
				t.Fatal("establishing the goal started a second turn")
			}
			// The supervisor's own view tracks it for restart accounting.
			if st, ok := s.Goal(); !ok || st.Status != GoalActive || st.Objective != g.Objective {
				t.Fatalf("observed goal state = %+v, ok=%t", st, ok)
			}
		})
	}
}

// An ordinary (non-coordinator) session never has its prompts turned into
// goals, whatever the runtime does.
func TestOrdinarySessionEstablishesNoGoal(t *testing.T) {
	s, fs := goalSession(t, false)
	peerTurn(s, "turn_a")
	userMessage(s, "turn_a", "item_1", "just a task")
	settle()
	if _, ok := fs.sawCall("thread/goal/set"); ok {
		t.Fatal("non-goal session set a goal")
	}
}

// Each status the goal machine can be in decides what a further task does.
func TestFurtherTaskHonorsGoalStatus(t *testing.T) {
	const objective = "original objective"
	existing := func(status string) *threadGoal {
		budget := int64(9000)
		return &threadGoal{ThreadID: "thr_1", Objective: objective, Status: status,
			TokenBudget: &budget, TokensUsed: 4242, CreatedAt: 77}
	}
	for _, tc := range []struct {
		status    string
		wantSet   bool
		wantClear bool
		// after a set, what the goal must look like
		wantObjective string
		wantStatus    string
		keepUsage     bool
	}{
		{status: GoalActive, wantSet: false},
		{status: GoalPaused, wantSet: false},
		{status: GoalBudgetLimited, wantSet: false},
		{status: GoalBlocked, wantSet: true, wantObjective: objective, wantStatus: GoalActive, keepUsage: true},
		{status: GoalUsageLimited, wantSet: true, wantObjective: objective, wantStatus: GoalActive, keepUsage: true},
		{status: GoalComplete, wantSet: true, wantClear: true, wantObjective: "a brand new task", wantStatus: GoalActive},
	} {
		t.Run(tc.status, func(t *testing.T) {
			s, fs := goalSessionWith(t, true, existing(tc.status))
			peerTurn(s, "turn_b")
			userMessage(s, "turn_b", "item_2", "a brand new task")
			settle()
			_, set := fs.sawCall("thread/goal/set")
			_, cleared := fs.sawCall("thread/goal/clear")
			if set != tc.wantSet || cleared != tc.wantClear {
				t.Fatalf("set=%t clear=%t, want set=%t clear=%t", set, cleared, tc.wantSet, tc.wantClear)
			}
			g := fs.goalState()
			if !tc.wantSet {
				if g == nil || g.Status != tc.status || g.Objective != objective || g.TokensUsed != 4242 {
					t.Fatalf("explicit/active goal was disturbed: %+v", g)
				}
				return
			}
			if g == nil || g.Objective != tc.wantObjective || g.Status != tc.wantStatus {
				t.Fatalf("goal = %+v, want %q/%s", g, tc.wantObjective, tc.wantStatus)
			}
			if tc.keepUsage {
				// A resumed goal keeps its objective, budget and accounting: only
				// the status changes.
				if g.TokensUsed != 4242 || g.TokenBudget == nil || *g.TokenBudget != 9000 || g.CreatedAt != 77 {
					t.Fatalf("resume reset goal accounting: %+v", g)
				}
				return
			}
			// A genuinely new task after completion starts fresh accounting rather
			// than inheriting the finished goal's usage, budget or creation time.
			if g.TokensUsed != 0 || g.TokenBudget != nil || g.CreatedAt == 77 {
				t.Fatalf("new goal inherited the completed goal's accounting: %+v", g)
			}
		})
	}
}

// A user `stop` that lands while the replacement of a completed goal is still
// in flight must win: the interrupted turn's task is abandoned, not activated.
func TestStopDuringCompletedGoalReplacementAbandonsTask(t *testing.T) {
	s, fs := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Objective: "finished work", Status: GoalComplete, CreatedAt: 5})
	release := fs.holdResponse("thread/goal/clear")
	peerTurn(s, "turn_c")
	userMessage(s, "turn_c", "item_3", "a task the user then stops")
	if !fs.awaitCall("thread/goal/clear", 2*time.Second) {
		t.Fatal("replacement never reached the clear step")
	}
	// The stop: Codex reports the interrupted turn while the clear is pending.
	endTurn(s, "turn_c", "interrupted")
	close(release)
	settle()
	if _, set := fs.sawCall("thread/goal/set"); set {
		t.Fatal("a stopped task still activated a new goal")
	}
	if g := fs.goalState(); g != nil {
		t.Fatalf("goal after abandoned replacement = %+v, want none", g)
	}
}

// The same rule holds for the single-step case: a stop while the set is in
// flight leaves no activated goal behind it.
func TestStopDuringFirstTaskLeavesNoActiveGoal(t *testing.T) {
	s, fs := goalSession(t, true)
	release := fs.holdResponse("thread/goal/get")
	peerTurn(s, "turn_d")
	userMessage(s, "turn_d", "item_4", "a task the user stops immediately")
	if !fs.awaitCall("thread/goal/get", 2*time.Second) {
		t.Fatal("observer never read the goal")
	}
	endTurn(s, "turn_d", "interrupted")
	close(release)
	settle()
	if _, set := fs.sawCall("thread/goal/set"); set {
		t.Fatal("a stopped first task still established a goal")
	}
}

// A user clear (or any native change) that arrives while the observer's set is
// in flight is the fresher evidence: the stale RPC result must not overwrite it.
func TestNewerNativeUpdateWinsOverInFlightSetResult(t *testing.T) {
	s, fs := goalSession(t, true)
	release := fs.holdResponse("thread/goal/set")
	peerTurn(s, "turn_e")
	userMessage(s, "turn_e", "item_5", "first task")
	if !fs.awaitCall("thread/goal/set", 2*time.Second) {
		t.Fatal("observer never issued the set")
	}
	// The user clears the goal while the set's reply is still held.
	cleared, _ := json.Marshal(map[string]any{"threadId": s.ThreadID()})
	s.onNotify("thread/goal/cleared", cleared)
	close(release)
	settle()
	if st, ok := s.Goal(); ok {
		t.Fatalf("stale set result overwrote the newer clear: %+v", st)
	}
}

// The same user message observed twice (item/started then item/completed, or a
// client re-reading history) establishes the goal exactly once.
func TestRepeatedObservationEstablishesOneGoal(t *testing.T) {
	s, fs := goalSession(t, true)
	peerTurn(s, "turn_f")
	for i := 0; i < 3; i++ {
		userMessage(s, "turn_f", "item_6", "one task")
	}
	awaitGoal(t, fs, func(g *threadGoal) bool { return g != nil }, 2*time.Second)
	settle()
	if n := len(fs.callsOf("thread/goal/set")); n != 1 {
		t.Fatalf("thread/goal/set calls = %d, want 1", n)
	}
}

// Messages outside a live turn (history replayed on attach, a foreign thread)
// are not tasks.
func TestOnlyLiveTurnMessagesAreTasks(t *testing.T) {
	s, fs := goalSession(t, true)
	userMessage(s, "", "item_7", "history with no live turn")
	foreign, _ := json.Marshal(map[string]any{
		"threadId": "someone-else", "turnId": "turn_g",
		"item": map[string]any{"id": "item_8", "type": "userMessage",
			"content": []map[string]any{{"type": "text", "text": "another session's task"}}},
	})
	s.onNotify("item/started", foreign)
	settle()
	if _, set := fs.sawCall("thread/goal/set"); set {
		t.Fatal("a message outside this thread's live turn became a goal")
	}
}

// A goal session on a Codex without the goals feature fails loudly instead of
// silently running as an ordinary session.
func TestGoalSessionRefusesToStartWithoutNativeGoals(t *testing.T) {
	for _, goals := range []bool{true, false} {
		name := "goal session"
		if !goals {
			name = "ordinary session"
		}
		t.Run(name, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, goalsDisabled: true}
			go fs.loop()
			defer fs.close()
			s := New(Config{SessionID: "coordinator", Goals: goals})
			defer s.Close()
			err := s.attach(context.Background(), client)
			if goals {
				if err == nil || !strings.Contains(err.Error(), "native goals unsupported") {
					t.Fatalf("goal session start = %v, want ErrGoalsUnsupported", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("goals disabled broke an ordinary session: %v", err)
			}
		})
	}
}

// The explicit goal verb is the user's decision: it pauses, resumes (including
// from budgetLimited, with a raised budget) and clears, and validates input.
func TestExplicitGoalControl(t *testing.T) {
	budget := int64(1000)
	s, fs := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Objective: "work", Status: GoalBudgetLimited,
		TokenBudget: &budget, TokensUsed: 1000, CreatedAt: 9})
	ctx := context.Background()
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalActive, TokenBudget: 5000}); err != nil {
		t.Fatalf("resume with a raised budget: %v", err)
	}
	g := fs.goalState()
	if g == nil || g.Status != GoalActive || g.TokenBudget == nil || *g.TokenBudget != 5000 || g.TokensUsed != 1000 || g.Objective != "work" {
		t.Fatalf("explicit resume = %+v", g)
	}
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalPaused}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if g := fs.goalState(); g == nil || g.Status != GoalPaused {
		t.Fatalf("explicit pause = %+v", g)
	}
	// A paused goal is the user's decision: a further task must not resume it.
	peerTurn(s, "turn_h")
	userMessage(s, "turn_h", "item_9", "keep going please")
	settle()
	if g := fs.goalState(); g == nil || g.Status != GoalPaused {
		t.Fatalf("a task resumed an explicitly paused goal: %+v", g)
	}
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalClear}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if g := fs.goalState(); g != nil {
		t.Fatalf("explicit clear left %+v", g)
	}
	if _, ok := s.Goal(); ok {
		t.Fatal("cleared goal still observed locally")
	}
	for _, u := range []GoalUpdate{{}, {Status: "nonsense"}, {Status: GoalActive}} {
		if err := s.SetGoal(ctx, u); err == nil {
			t.Fatalf("SetGoal(%+v) was accepted", u)
		}
	}
}

// A goal the model finishes while the observer's read is still in flight must
// not be resurrected: the task was admitted against the goal state at the time
// its message was observed, and that state has moved on.
func TestGoalFinishedDuringPendingReadIsNotResurrected(t *testing.T) {
	s, fs := goalSession(t, true)
	release := fs.holdResponse("thread/goal/get")
	peerTurn(s, "turn_i")
	userMessage(s, "turn_i", "item_10", "run the release checklist")
	if !fs.awaitCall("thread/goal/get", 2*time.Second) {
		t.Fatal("observer never read the goal")
	}
	// The model creates the goal itself and completes it within the same turn.
	goal := &threadGoal{ThreadID: s.ThreadID(), Objective: "run the release checklist", Status: GoalActive, CreatedAt: 42}
	fs.setGoalState(goal)
	notifyGoal(s, goal)
	done := *goal
	done.Status = GoalComplete
	fs.setGoalState(&done)
	notifyGoal(s, &done)
	endTurn(s, "turn_i", "completed")
	close(release)
	settle()
	if _, cleared := fs.sawCall("thread/goal/clear"); cleared {
		t.Fatal("a finished goal was cleared for replacement by its own task")
	}
	if _, set := fs.sawCall("thread/goal/set"); set {
		t.Fatal("a just-completed task was restarted as a new goal")
	}
	if g := fs.goalState(); g == nil || g.Status != GoalComplete {
		t.Fatalf("goal = %+v, want the completed goal untouched", g)
	}
}

// A user clear that lands while the observer's read is in flight wins too, even
// though it leaves the goal nil — the state the task was admitted against
// (a completed goal to replace) is gone, so nothing is established.
func TestExternalClearDuringPendingReadDropsTask(t *testing.T) {
	s, fs := goalSessionWith(t, true, &threadGoal{ThreadID: "thr_1", Objective: "finished work", Status: GoalComplete, CreatedAt: 11})
	release := fs.holdResponse("thread/goal/get")
	peerTurn(s, "turn_j")
	userMessage(s, "turn_j", "item_11", "next piece of work")
	if !fs.awaitCall("thread/goal/get", 2*time.Second) {
		t.Fatal("observer never read the goal")
	}
	fs.setGoalState(nil)
	cleared, _ := json.Marshal(map[string]any{"threadId": s.ThreadID()})
	s.onNotify("thread/goal/cleared", cleared)
	close(release)
	settle()
	if _, set := fs.sawCall("thread/goal/set"); set {
		t.Fatal("a task admitted against a since-cleared goal still set one")
	}
	if g := fs.goalState(); g != nil {
		t.Fatalf("goal = %+v, want none after the user's clear", g)
	}
}

// notifyGoal delivers the broadcast the App Server sends every client when a
// goal changes — here, one amux did not make.
func notifyGoal(s *Supervisor, g *threadGoal) {
	params, _ := json.Marshal(map[string]any{"threadId": s.ThreadID(), "goal": g})
	s.onNotify("thread/goal/updated", params)
}

// A long-lived coordinator's bookkeeping is bounded, but eviction must never
// forget a message whose own turn is still live: the App Server broadcasts the
// same userMessage twice (item/started, then item/completed), so a forgotten id
// would be admitted a second time and re-establish work that just completed.
func TestTaskHistoryEvictionKeepsLiveTurnMessages(t *testing.T) {
	s, fs := goalSession(t, true)
	// Old, finished turns fill the bookkeeping past its bound. The maps are
	// created lazily by the observer, so the fixture makes them itself.
	seedHistory(s, maxTurnHistory+16)

	peerTurn(s, "turn_k")
	userMessage(s, "turn_k", "item_live", "the live task")
	awaitGoal(t, fs, func(g *threadGoal) bool { return g != nil }, 2*time.Second)
	settle() // the observer has finished deciding; eviction has run

	// The model finishes the goal inside that same turn.
	done := *fs.goalState()
	done.Status = GoalComplete
	fs.setGoalState(&done)
	notifyGoal(s, &done)

	// item/completed for the SAME message now arrives.
	userMessage(s, "turn_k", "item_live", "the live task")
	settle()

	if n := len(fs.callsOf("thread/goal/set")); n != 1 {
		t.Fatalf("thread/goal/set calls = %d, want 1 (the second observation was re-admitted)", n)
	}
	if _, cleared := fs.sawCall("thread/goal/clear"); cleared {
		t.Fatal("the completed goal was replaced by a repeat of its own message")
	}
	if g := fs.goalState(); g == nil || g.Status != GoalComplete {
		t.Fatalf("goal = %+v, want the completed goal untouched", g)
	}
	// Eviction still did its job on the entries nothing refers to.
	s.mu.Lock()
	kept, tasks := s.goalTasks["item_live"], len(s.goalTasks)
	s.mu.Unlock()
	if kept != "turn_k" {
		t.Fatalf("live message id was evicted: %q", kept)
	}
	if tasks > maxTurnHistory+1 {
		t.Fatalf("task history unbounded: %d entries", tasks)
	}
}

// seedHistory fills a supervisor's task/turn bookkeeping with finished turns,
// as a long-lived coordinator accumulates them. It creates the maps the
// observer would otherwise create lazily, so the fixture never writes into a
// nil map while holding the lock.
func seedHistory(s *Supervisor, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goalTasks == nil {
		s.goalTasks = map[string]string{}
	}
	if s.endedTurns == nil {
		s.endedTurns = map[string]string{}
	}
	for i := 0; i < n; i++ {
		turn := fmt.Sprintf("old_turn_%d", i)
		s.goalTasks[fmt.Sprintf("old_item_%d", i)] = turn
		s.endedTurns[turn] = "completed"
	}
}
