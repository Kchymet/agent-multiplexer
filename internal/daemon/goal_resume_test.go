package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/store"
)

// putCoordinator stores a Claude workgroup coordinator (a root session on the
// hook-goal runtime) whose session directory can hold a goal file.
func putCoordinator(t *testing.T, id, kind string) store.Session {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	dir := store.CoordinatorDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: id, Name: id, Scope: store.ScopeWork, Agent: kind, Dir: dir, ClaudeID: convID(id), Created: 1}
	if err := db.PutSession(s); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if kind == "claude" && !agent.HookGoals(s) {
		t.Fatal("fixture is not a hook-goal coordinator")
	}
	return s
}

func writeGoalFile(t *testing.T, s store.Session, state claudegoal.State) {
	t.Helper()
	path := claudegoal.StatePath(s.Dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, claudegoal.Encode(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readGoalFile(t *testing.T, s store.Session) claudegoal.State {
	t.Helper()
	b, err := os.ReadFile(claudegoal.StatePath(s.Dir))
	if err != nil {
		t.Fatal(err)
	}
	state, err := claudegoal.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// captureLaunch records the restart intent each launch of the agent tab carries.
type launchIntent struct {
	resumeWork   bool
	resumePrompt string
}

func captureLaunch(d *Daemon, kind string, got *[]launchIntent) {
	d.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
		if tab == panespec.TabAgent {
			*got = append(*got, launchIntent{spec.ResumeWork, spec.ResumePrompt})
		}
		return "", nil, []string{kind}, nil
	}
}

// Every way a Claude coordinator's runtime comes back — an agent restart, the
// daemon's own restore after its restart, `amux do start` — relaunches an
// active or waiting goal with its continuation and no user turn, whatever the
// hook state was (an idle coordinator waiting on a background wake-up reads
// ready, and its wake-up died with the runtime). Paused, complete, blocked,
// input-waiting and budget-limited goals are left to the user: the launch then
// follows the running-work rule alone.
func TestRestartResumesEligibleClaudeGoals(t *testing.T) {
	type goalCase struct {
		name  string
		state claudegoal.State
		want  bool
	}
	cases := []goalCase{
		{"active", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive, Continuations: 4}, true},
		{"waiting", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusWaiting, Note: "CI"}, true},
		{"paused", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusPaused}, false},
		{"input", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusInput, Note: "which region?"}, false},
		{"blocked", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusBlocked}, false},
		{"budgetLimited", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusBudgetLimited}, false},
		{"complete", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusComplete}, false},
		{"spent", claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive, Continuations: claudegoal.MaxContinuations}, false},
		{"none", claudegoal.State{}, false},
	}
	for _, tc := range cases {
		for _, hook := range []string{core.StateReady, core.StateRunning} {
			t.Run(tc.name+"/"+hook, func(t *testing.T) {
				d, e := steerDaemon(t)
				d.goalAutoResume = true
				sess := putCoordinator(t, "wg1", "claude")
				if !tc.state.None() {
					writeGoalFile(t, sess, tc.state)
				}
				e.running("wg1")
				if err := core.WriteSessionHookState("wg1", convID("wg1"), hook, ""); err != nil {
					t.Fatal(err)
				}
				wantResume := tc.want || hook == core.StateRunning
				var launches []launchIntent
				captureLaunch(d, "claude", &launches)

				// 1. An agent restart.
				if r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "wg1"}); !r.OK {
					t.Fatal(r.Error)
				}
				// 2. The daemon's restore of its journal after its own restart. The
				// exit hook of the old runtime marks it idle before the relaunch.
				d.liveAgentsPath = filepath.Join(t.TempDir(), "live.json")
				d.persistLiveAgents()
				d.pendingRestore = d.readLiveAgents()
				core.WriteSessionHookState("wg1", convID("wg1"), core.StateIdle, "")
				e.Kill(engine.Key{AgentID: "wg1", Tab: panespec.TabAgent})
				d.sessions = []core.Session{{ID: "wg1"}}
				d.restoreLiveAgents(context.Background())
				// 3. `amux do start` of the stopped coordinator.
				e.Kill(engine.Key{AgentID: "wg1", Tab: panespec.TabAgent})
				if r := d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "wg1"}); !r.OK {
					t.Fatal(r.Error)
				}

				if len(launches) != 3 {
					t.Fatalf("launches=%d", len(launches))
				}
				for i, l := range launches {
					// The restart and the journal restore both saw the hook state;
					// a plain start has no journal, so only the goal file can ask
					// for a continuation.
					want := wantResume
					if i == 2 {
						want = tc.want
					}
					if l.resumeWork != want {
						t.Errorf("launch %d: resumeWork=%v want %v", i, l.resumeWork, want)
					}
					if l.resumePrompt != claudegoal.RestartPrompt {
						t.Errorf("launch %d: a goal coordinator got the generic continuation %q", i, l.resumePrompt)
					}
				}
				// The decision reads the file; it never moves it (the prompt hook
				// does, when the continuation is actually submitted).
				if !tc.state.None() && readGoalFile(t, sess) != tc.state {
					t.Fatal("restart intent rewrote the goal file")
				}
			})
		}
	}
}

// goal.auto_resume off keeps the running-work rule only. The continuation a
// goal coordinator is given is still the goal-aware one, so a restart mid-turn
// can never be recorded as the coordinator's new task.
func TestGoalAutoResumeSwitchOff(t *testing.T) {
	d, e := steerDaemon(t)
	d.goalAutoResume = false
	sess := putCoordinator(t, "wg1", "claude")
	writeGoalFile(t, sess, claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive})
	e.running("wg1")
	for _, hook := range []string{core.StateReady, core.StateRunning} {
		if err := core.WriteSessionHookState("wg1", convID("wg1"), hook, ""); err != nil {
			t.Fatal(err)
		}
		var launches []launchIntent
		captureLaunch(d, "claude", &launches)
		if r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "wg1"}); !r.OK {
			t.Fatal(r.Error)
		}
		if len(launches) != 1 || launches[0].resumeWork != (hook == core.StateRunning) || launches[0].resumePrompt != claudegoal.RestartPrompt {
			t.Fatalf("%s: %+v", hook, launches)
		}
	}
}

// Goal auto-resume is the coordinator's: an ordinary Claude member with the
// same file in its directory, and a Codex coordinator (whose goal is the App
// Server's), keep the plain running-work rule and the generic continuation.
func TestGoalAutoResumeOnlyForHookGoalCoordinators(t *testing.T) {
	d, e := steerDaemon(t)
	d.goalAutoResume = true
	putSession(t, "m1", "claude")
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	member, _, _ := db.GetSession("m1")
	db.Close()
	writeGoalFile(t, member, claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive})
	codex := putCoordinator(t, "wg2", "codex")
	writeGoalFile(t, codex, claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive})
	for id, kind := range map[string]string{"m1": "claude", "wg2": "codex"} {
		e.running(id)
		if err := core.WriteSessionHookState(id, convID(id), core.StateReady, ""); err != nil {
			t.Fatal(err)
		}
		var launches []launchIntent
		captureLaunch(d, kind, &launches)
		if r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: id}); !r.OK {
			t.Fatal(r.Error)
		}
		if len(launches) != 1 || launches[0].resumeWork || launches[0].resumePrompt != "" {
			t.Fatalf("%s: %+v", id, launches)
		}
	}
}

// A `prompt` that cold-starts a stopped coordinator is itself the turn (and the
// prompt hook makes it the goal): the start behind it submits no continuation
// of its own, so the coordinator is not handed two competing submissions.
func TestPromptColdStartDoesNotAlsoResumeGoal(t *testing.T) {
	d, e := steerDaemon(t)
	d.goalAutoResume = true
	sess := putCoordinator(t, "wg1", "claude")
	writeGoalFile(t, sess, claudegoal.State{Objective: "ship it", Status: claudegoal.StatusActive})
	var launches []launchIntent
	captureLaunch(d, "claude", &launches)
	if err := d.steer(context.Background(), core.Action{ID: "wg1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "new task"}}); err != nil {
		t.Fatal(err)
	}
	waitStarted(t, d)
	if len(launches) != 1 || launches[0].resumeWork {
		t.Fatalf("launches: %+v", launches)
	}
	in, ok := e.Lookup(engine.Key{AgentID: "wg1", Tab: panespec.TabAgent})
	if !ok {
		t.Fatal("agent not started")
	}
	if w := in.(*fakeInstance).written(); !containsAll(w, "new task") || containsAll(w, claudegoal.RestartPrompt) {
		t.Fatalf("written: %q", w)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
