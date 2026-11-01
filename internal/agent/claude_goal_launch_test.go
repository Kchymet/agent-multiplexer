package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"amux/internal/claudegoal"
	"amux/internal/hostprep"
	"amux/internal/store"
)

func freshLaunch(t *testing.T, s store.Session, prompt string) (LaunchDecision, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	s.Dir = dir
	root, err := hostprep.OpenSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := HarnessFor(s.Agent).PlanLaunch(LaunchRequest{Root: root, Session: s, Dir: dir, Prompt: prompt})
	if err != nil {
		t.Fatalf("PlanLaunch: %v", err)
	}
	return got, dir
}

func readGoal(t *testing.T, dir string) claudegoal.State {
	t.Helper()
	b, err := os.ReadFile(claudegoal.StatePath(dir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	st, err := claudegoal.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestClaudeMemberLaunchesWithNativeGoal is the regression for Claude members
// starting with no goal: a fresh Claude task-mode member is launched with its
// task as Claude Code's native /goal, not as a plain first turn.
func TestClaudeMemberLaunchesWithNativeGoal(t *testing.T) {
	member := store.Session{ID: "m1", RootID: "r1", Agent: "claude", Mode: store.ModeTask, ClaudeID: "c-1"}
	got, dir := freshLaunch(t, member, "fix the bug\n\nand open a PR")
	want := []string{"--session-id", "c-1", "/goal fix the bug\n\nand open a PR"}
	if !reflect.DeepEqual(got.Extra, want) {
		t.Fatalf("member extra = %q, want %q", got.Extra, want)
	}
	if st := readGoal(t, dir); !st.None() {
		t.Fatalf("a member must not get amux's hook goal file, got %+v", st)
	}

	// Blank mode is task mode.
	member.Mode, member.ClaudeID = "", ""
	if got, _ := freshLaunch(t, member, "do it"); !reflect.DeepEqual(got.Extra, []string{"/goal do it"}) {
		t.Fatalf("blank-mode member extra = %q, want [/goal do it]", got.Extra)
	}
}

// TestClaudeMemberLongTaskGoal: a task over Claude's /goal condition limit
// would be refused by /goal (no goal at all), so it is written to the session's
// private state and the goal points at it.
func TestClaudeMemberLongTaskGoal(t *testing.T) {
	task := strings.Repeat("é", MaxNativeGoalCondition+1)
	member := store.Session{ID: "m1", RootID: "r1", Agent: "claude", Mode: store.ModeTask}
	got, dir := freshLaunch(t, member, task)
	if len(got.Extra) != 1 || !strings.HasPrefix(got.Extra[0], "/goal ") {
		t.Fatalf("extra = %q, want one /goal prompt", got.Extra)
	}
	cond := strings.TrimPrefix(got.Extra[0], "/goal ")
	path := filepath.Join(dir, NativeGoalTaskRel)
	if len(cond) > MaxNativeGoalCondition || !strings.Contains(cond, path) {
		t.Fatalf("condition %q must fit the limit and name %s", cond, path)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != task {
		t.Fatalf("task file = %d bytes, %v; want the full task", len(b), err)
	}
}

// TestClaudeNonGoalLaunchesUnchanged: interactive members and other harnesses
// keep the plain positional prompt — Codex in particular is unaffected.
func TestClaudeNonGoalLaunchesUnchanged(t *testing.T) {
	for _, s := range []store.Session{
		{ID: "m1", RootID: "r1", Agent: "claude", Mode: store.ModeInteractive},
		{ID: "m2", RootID: "r1", Agent: "codex", Mode: store.ModeTask},
		{ID: "r2", Agent: "codex", Mode: store.ModeTask}, // codex coordinator
		{ID: "m3", RootID: "r1", Agent: "hermes", Mode: store.ModeTask},
	} {
		got, _ := freshLaunch(t, s, "do it")
		if !reflect.DeepEqual(got.Extra, []string{"do it"}) {
			t.Errorf("%s/%s extra = %q, want [do it]", s.Agent, s.Mode, got.Extra)
		}
	}
}

// TestClaudeCoordinatorGoalSeededAtLaunch is the regression for Claude
// coordinators starting with no goal: the task is the active goal on file
// before Claude runs, rather than depending on UserPromptSubmit firing for the
// positional launch prompt.
func TestClaudeCoordinatorGoalSeededAtLaunch(t *testing.T) {
	coord := store.Session{ID: "r1", Agent: "claude", Mode: store.ModeTask}
	got, dir := freshLaunch(t, coord, "ship the feature")
	if !reflect.DeepEqual(got.Extra, []string{"ship the feature"}) {
		t.Fatalf("coordinator extra = %q, want the plain task (amux goal hooks, not native /goal)", got.Extra)
	}
	st := readGoal(t, dir)
	if st.Status != claudegoal.StatusActive || st.Objective != "ship the feature" {
		t.Fatalf("seeded goal = %+v, want active with the task", st)
	}
	if !claudegoal.ContinueOnRestart(st) {
		t.Fatal("a seeded goal must continue across a restart")
	}
}

// TestClaudeCoordinatorSeedKeepsExistingGoal: a fresh start that replays the
// creation prompt (a lost transcript) must not overwrite a goal already on
// file.
func TestClaudeCoordinatorSeedKeepsExistingGoal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	prev := claudegoal.State{Objective: "the newer task", Status: claudegoal.StatusPaused}
	if err := os.MkdirAll(filepath.Dir(claudegoal.StatePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudegoal.StatePath(dir), claudegoal.Encode(prev), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := hostprep.OpenSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	coord := store.Session{ID: "r1", Agent: "claude", Mode: store.ModeTask, Dir: dir}
	if _, err := HarnessFor("claude").PlanLaunch(LaunchRequest{Root: root, Session: coord, Dir: dir, Prompt: "original task"}); err != nil {
		t.Fatal(err)
	}
	if st := readGoal(t, dir); st.Objective != prev.Objective || st.Status != prev.Status {
		t.Fatalf("goal = %+v, want the existing %+v kept", st, prev)
	}
}
