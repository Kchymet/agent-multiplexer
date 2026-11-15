package codexapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// observeGoalUpdate feeds s one thread/goal/updated notification on its read path.
func observeGoalUpdate(s *Supervisor, g *threadGoal) {
	b, _ := json.Marshal(map[string]any{"threadId": g.ThreadID, "goal": g})
	s.onNotify("thread/goal/updated", b)
}

// resumeWith starts a supervisor on a resumed thread whose server-side goal is
// serverGoal, and reports the thread/goal/set it sent (if any).
func resumeWith(t *testing.T, cfg Config, serverGoal *threadGoal) (*Supervisor, *fakeServer, map[string]any) {
	t.Helper()
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	if serverGoal != nil {
		g := *serverGoal
		fs.goal = &g
	}
	go fs.loop()
	t.Cleanup(fs.close)
	s := New(cfg)
	t.Cleanup(func() { s.Close() })
	attach(t, s, client)
	call, ok := fs.sawCall("thread/goal/set")
	if !ok {
		return s, fs, nil
	}
	var p map[string]any
	_ = json.Unmarshal(call.Params, &p)
	return s, fs, p
}

// TestDaemonRestartRestoresActiveGoalFromDurableIntent covers a daemon restart
// in which the launch carries no restart-journal evidence at all (an attach,
// prompt or `amux do start` reached the runtime before restore, or the journal
// entry was lost): the durable goal-intent record alone decides.
func TestDaemonRestartRestoresActiveGoalFromDurableIntent(t *testing.T) {
	const thread = "thr_saved"
	saved := threadGoal{ThreadID: thread, Objective: "keep working", Status: GoalActive, CreatedAt: 7}
	for _, tc := range []struct {
		name string
		// before is the sequence of statuses amux observed live before shutdown.
		before   []string
		after    string // the status the resumed thread reports
		other    bool   // the resumed thread reports a different goal
		journal  *RestartWork
		wantSet  bool
		wantKept string // status the intent should leave recorded after restore ("" = no active key)
	}{
		{name: "active, paused while unobserved", before: []string{GoalActive}, after: GoalPaused, wantSet: true, wantKept: GoalActive},
		{name: "active stays active", before: []string{GoalActive}, after: GoalActive, wantKept: GoalActive},
		{name: "user paused while observed", before: []string{GoalActive, GoalPaused}, after: GoalPaused},
		{name: "user pause beats a stale journal", before: []string{GoalActive, GoalPaused}, after: GoalPaused,
			journal: &RestartWork{ThreadID: thread, GoalKey: saved.key()}},
		{name: "blocked is never reactivated", before: []string{GoalActive}, after: GoalBlocked},
		{name: "usage limited is never reactivated", before: []string{GoalActive}, after: GoalUsageLimited},
		{name: "budget limited is never reactivated", before: []string{GoalActive}, after: GoalBudgetLimited},
		{name: "complete is never reactivated", before: []string{GoalActive}, after: GoalComplete},
		{name: "a different goal is never reactivated", before: []string{GoalActive}, after: GoalPaused, other: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "goal.json")
			// Phase 1: the pre-restart supervisor observes the goal live.
			first, _, _ := resumeWith(t, Config{SessionID: "g", ResumeThreadID: thread, GoalIntentPath: path}, nil)
			for _, st := range tc.before {
				g := saved
				g.Status = st
				observeGoalUpdate(first, &g)
			}
			first.interruptTransport() // daemon shutdown
			// A pause landing during shutdown is not observed and not recorded.
			g := saved
			g.Status = GoalPaused
			observeGoalUpdate(first, &g)
			first.Close()

			// Phase 2: the restarted daemon resumes the same thread.
			after := saved
			after.Status = tc.after
			if tc.other {
				after.Objective = "a different goal"
			}
			_, fs, set := resumeWith(t, Config{SessionID: "g", ResumeThreadID: thread, GoalIntentPath: path, RestartWork: tc.journal}, &after)
			if (set != nil) != tc.wantSet {
				t.Fatalf("thread/goal/set sent=%t (%v), want %t", set != nil, set, tc.wantSet)
			}
			if set != nil {
				if len(set) != 2 || set["threadId"] != thread || set["status"] != GoalActive {
					t.Fatalf("restore changed more than status: %v", set)
				}
			}
			if _, ok := fs.sawCall("turn/start"); ok {
				t.Fatal("goal restore started a turn; Codex continues an active goal itself")
			}
			gi, ok := loadGoalIntent(path)
			if !ok || gi.ThreadID != thread {
				t.Fatalf("intent record = %+v ok=%t", gi, ok)
			}
			if want := tc.wantKept == GoalActive; (gi.ActiveGoalKey == saved.key()) != want {
				t.Fatalf("recorded active key = %q, want active=%t", gi.ActiveGoalKey, want)
			}
		})
	}
}

// TestGoalIntentIgnoresUnobservedAndRecordsObservedChanges pins when the durable
// record moves: every live observed change, never one after shutdown began.
func TestGoalIntentIgnoresUnobservedAndRecordsObservedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.json")
	s, _, _ := resumeWith(t, Config{SessionID: "g", ResumeThreadID: "thr_1", GoalIntentPath: path}, nil)
	goal := threadGoal{ThreadID: "thr_1", Objective: "task", Status: GoalActive, CreatedAt: 1}
	key := func() string { gi, _ := loadGoalIntent(path); return gi.ActiveGoalKey }
	observeGoalUpdate(s, &goal)
	if key() != goal.key() {
		t.Fatal("active goal not recorded")
	}
	goal.Status = GoalPaused
	foreign := goal
	foreign.ThreadID = "other"
	observeGoalUpdate(s, &foreign)
	if key() != goal.key() {
		t.Fatal("a foreign thread's pause changed the record")
	}
	observeGoalUpdate(s, &goal)
	if key() != "" {
		t.Fatal("an observed pause was not recorded")
	}
	goal.Status = GoalActive
	observeGoalUpdate(s, &goal)
	s.interruptTransport()
	goal.Status = GoalPaused
	observeGoalUpdate(s, &goal)
	if key() != goal.key() {
		t.Fatal("a pause during shutdown overwrote the record")
	}
}

// goalServers is an httptest App Server endpoint whose n-th connection (from 0)
// starts with goals[n] as its thread goal.
func goalServers(t *testing.T, goals ...*threadGoal) (string, chan *fakeServer) {
	t.Helper()
	servers := make(chan *fakeServer, len(goals)+1)
	var mu sync.Mutex
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		mu.Lock()
		if n < len(goals) && goals[n] != nil {
			g := *goals[n]
			fs.goal = &g
		}
		n++
		mu.Unlock()
		defer fs.close()
		servers <- fs
		fs.loop()
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), servers
}

func goalSet(fs *fakeServer) (map[string]any, bool) {
	call, ok := fs.sawCall("thread/goal/set")
	if !ok {
		return nil, false
	}
	var p map[string]any
	_ = json.Unmarshal(call.Params, &p)
	return p, true
}

func waitTransportDown(t *testing.T, sup *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for sup.live() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sup.live() {
		t.Fatal("transport did not close")
	}
}

// TestDisconnectResumeRestoresActiveGoal covers a session disconnect: the
// daemon's transport to a still-running App Server drops, and the goal comes
// back paused by a client amux could not observe meanwhile. The existing
// on-demand reconnect restores it as active; a goal the user paused while amux
// was watching stays paused. Nothing here reconnects on its own — the reconnect
// happens only because the test calls Ensure, as an attach or prompt would.
func TestDisconnectResumeRestoresActiveGoal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  []string
		wantSet bool
	}{
		{"active before the gap", []string{GoalActive}, true},
		{"user paused before the gap", []string{GoalActive, GoalPaused}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			goal := threadGoal{ThreadID: "thr_1", Objective: "ship it", Status: GoalActive, CreatedAt: 3}
			paused := goal
			paused.Status = GoalPaused
			endpoint, servers := goalServers(t, nil, &paused)
			m := NewManager(context.Background(), "")
			defer m.Shutdown()
			ensure := func() (*Supervisor, error) {
				return m.Ensure(context.Background(), "gap", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
			}
			sup, err := ensure()
			if err != nil {
				t.Fatal(err)
			}
			first := <-servers
			for _, st := range tc.before {
				g := goal
				g.Status = st
				first.pushNotify("thread/goal/updated", map[string]any{"threadId": "thr_1", "goal": g})
				if !awaitObservedGoal(sup, st, 2*time.Second) {
					t.Fatalf("goal %s never observed", st)
				}
			}
			first.close()
			waitTransportDown(t, sup)

			again, err := ensure()
			if err != nil {
				t.Fatalf("Ensure after disconnect: %v", err)
			}
			if again != sup {
				t.Fatal("disconnect recovery replaced the supervisor")
			}
			second := <-servers
			set, ok := goalSet(second)
			if ok != tc.wantSet {
				t.Fatalf("thread/goal/set sent=%t (%v), want %t", ok, set, tc.wantSet)
			}
			if ok && (set["status"] != GoalActive || len(set) != 2) {
				t.Fatalf("reconnect restore changed more than status: %v", set)
			}
		})
	}
}

// TestDisconnectWithNoGoalDoesNotReadGoal keeps an ordinary session's reconnect
// a pure transport recovery: no goal on either side, no goal RPC.
func TestDisconnectWithNoGoalDoesNotReadGoal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	endpoint, servers := goalServers(t, nil, nil)
	m := NewManager(context.Background(), "")
	defer m.Shutdown()
	ensure := func() (*Supervisor, error) {
		return m.Ensure(context.Background(), "plain", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
	}
	sup, err := ensure()
	if err != nil {
		t.Fatal(err)
	}
	(<-servers).close()
	waitTransportDown(t, sup)
	if _, err := ensure(); err != nil {
		t.Fatal(err)
	}
	second := <-servers
	if _, ok := second.sawCall("thread/goal/get"); ok {
		t.Fatal("goal-less reconnect read the goal")
	}
}

// TestRelaunchRestoresActiveGoal covers the two resumes that start a new App
// Server with no restart-journal evidence: a disconnect in which the App Server
// itself exited (the next Ensure relaunches it), and a daemon restart (a new
// manager). Both resume the pinned thread and restore the active goal.
func TestRelaunchRestoresActiveGoal(t *testing.T) {
	for _, mode := range []string{"process exited", "daemon restart"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			goal := threadGoal{ThreadID: "thr_1", Objective: "ship it", Status: GoalActive, CreatedAt: 3}
			paused := goal
			paused.Status = GoalPaused
			endpoint, servers := goalServers(t, nil, &paused)
			m := NewManager(context.Background(), "")
			ensure := func(m *Manager) (*Supervisor, error) {
				return m.Ensure(context.Background(), "relaunch", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
			}
			sup, err := ensure(m)
			if err != nil {
				t.Fatal(err)
			}
			first := <-servers
			first.pushNotify("thread/goal/updated", map[string]any{"threadId": "thr_1", "goal": goal})
			if !awaitObservedGoal(sup, GoalActive, 2*time.Second) {
				t.Fatal("active goal never observed")
			}
			if mode == "process exited" {
				defer m.Shutdown()
				sup.mu.Lock()
				proc := sup.proc
				sup.mu.Unlock()
				_ = proc.Process.Kill()
				deadline := time.Now().Add(2 * time.Second)
				for sup.processRunning() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			} else {
				m.Shutdown()
				m = NewManager(context.Background(), "")
				defer m.Shutdown()
			}
			next, err := ensure(m)
			if err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if next == sup {
				t.Fatal("expected a new supervisor")
			}
			second := <-servers
			if _, ok := second.sawCall("thread/resume"); !ok {
				t.Fatal("relaunch did not resume the pinned thread")
			}
			set, ok := goalSet(second)
			if !ok || set["status"] != GoalActive || len(set) != 2 {
				t.Fatalf("relaunch did not restore the active goal: sent=%t %v", ok, set)
			}
		})
	}
}
