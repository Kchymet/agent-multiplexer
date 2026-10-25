package wsops

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/store"
)

// A coordinator's guide must describe the lifecycle it actually has, and teach
// only the operations its mailbox grant authorizes: a guide that promises goal
// mode on a harness without one, or advertises a host-only operation, sends the
// model into failures the user then has to unpick.
func TestCoordinatorGuideDescribesItsRealLifecycle(t *testing.T) {
	root := store.Session{ID: "wg1", Name: "payments", Agent: agent.GoalRuntime, Dir: "/sandbox/wg1"}
	if !agent.NativeGoals(root) {
		t.Fatal("fixture is not a goal coordinator")
	}
	guide := coordinatorGuide(root)
	for _, want := range []string{
		"native", "update_goal", "complete",
		"request_user_input",             // ask only for genuine input
		"amux do steer wg1 -f verb=goal", // the user's pause/resume/clear
		"amux do add-agent wg1",
		"amux do set-archived",
		"choose the sensible default", // decide and keep going
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("goal coordinator guide missing %q", want)
		}
	}
	// Restoring an archived member is the user's operation, not the
	// coordinator's: its grant refuses it. The workgroup archive command is named
	// only as the user's host operation, never as a step the coordinator takes.
	if strings.Contains(guide, "amux do unarchive") {
		t.Error("coordinator guide advertises the host-only amux do unarchive")
	}
	assertCompletionLeavesWorkgroupOpen(t, "goal coordinator", guide, "wg1")

	// A Claude coordinator runs in goal mode by default, through amux's hooks:
	// its guide teaches the amux goal commands and how to get missing input
	// from the user, never Codex's update_goal.
	claude := store.Session{ID: "wg3", Name: "payments", Agent: agent.HookGoalRuntime, Dir: "/sandbox/wg3"}
	if !agent.HookGoals(claude) || !agent.GoalMode(claude) {
		t.Fatal("claude coordinator fixture is not in goal mode")
	}
	hooked := coordinatorGuide(claude)
	flatHooked := strings.Join(strings.Fields(hooked), " ")
	for _, want := range []string{
		"Goal mode (amux goal hooks)",
		"goal mode by default",
		"amux agent goal complete",
		"amux agent goal ask",
		"amux agent goal blocked",
		"AskUserQuestion",                                 // concrete choices
		"ask instead of guessing",                         // user-owned decisions
		"choose the sensible default",                     // optional details
		"amux do steer wg3 -f verb=goal -f status=paused", // the user's controls
	} {
		if !strings.Contains(flatHooked, want) {
			t.Errorf("claude goal coordinator guide missing %q", want)
		}
	}
	for _, forbidden := range []string{"update_goal", "no native goal engine", "request_user_input"} {
		if strings.Contains(hooked, forbidden) {
			t.Errorf("claude goal coordinator guide mentions %q", forbidden)
		}
	}
	assertCompletionLeavesWorkgroupOpen(t, "claude goal coordinator", hooked, "wg3")

	// A coordinator on a harness with neither goal mechanism is told the truth.
	plain := store.Session{ID: "wg2", Name: "payments", Agent: "hermes", Dir: "/sandbox/wg2"}
	if agent.GoalMode(plain) {
		t.Fatal("fixture unexpectedly has goal mode")
	}
	other := coordinatorGuide(plain)
	if !strings.Contains(other, "no native goal engine") {
		t.Error("non-goal coordinator guide claims or omits its lifecycle")
	}
	if strings.Contains(other, "update_goal") || strings.Contains(other, "amux agent goal") {
		t.Error("non-goal coordinator guide teaches a tool it does not have")
	}
	if !strings.Contains(other, agent.GoalRuntime) {
		t.Error("non-goal coordinator guide does not say which runtime does run goals")
	}
	assertCompletionLeavesWorkgroupOpen(t, "non-goal coordinator", other, "wg2")
}

// Finishing a task must never read as "the workgroup gets archived": on every
// harness the guide tells the coordinator the workgroup stays open, to ask the
// user whether to archive it or continue, and that archiving is the user's host
// operation — not something the coordinator (or a member on its behalf) does.
func assertCompletionLeavesWorkgroupOpen(t *testing.T, kind, guide, id string) {
	t.Helper()
	// The guide is hard-wrapped prose; match phrases against its unwrapped form.
	flat := strings.Join(strings.Fields(guide), " ")
	for _, want := range []string{
		"archives nothing",
		"ask the user whether to archive the workgroup",
		"amux workgroup archive " + id,
		"Never archive this workgroup yourself — not with `amux agent done`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("%s guide missing %q", kind, want)
		}
	}
	for _, forbidden := range []string{
		"automatically marks the workgroup done",
		"marks the workgroup done",
		"stops its coordinator and members",
	} {
		if strings.Contains(flat, forbidden) {
			t.Errorf("%s guide still promises auto-archive: %q", kind, forbidden)
		}
	}
}

// The member guide is unchanged by goal mode: an ordinary agent is dispatched by
// its coordinator and has no goal of its own to reason about.
func TestMemberGuideMentionsNoGoals(t *testing.T) {
	guide := memberGuide(store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Branch: "amux/wg1-m1"})
	for _, forbidden := range []string{"update_goal", "goal mode", "thread goal"} {
		if strings.Contains(guide, forbidden) {
			t.Errorf("member guide mentions %q", forbidden)
		}
	}
}

// The console dispatches creations for the user, so its guide must name the
// creation fields the daemon actually accepts — a guide that teaches a field
// the allowlist refuses sends the model into an error it cannot diagnose.
func TestConsoleGuideTeachesCoordinatorFields(t *testing.T) {
	guide := consoleGuide(store.Session{ID: "console", Agent: "claude", Dir: "/sandbox/console", Mode: store.ModeConsole})
	for _, want := range []string{FieldCoordinator, FieldCoordinatorModel, agent.GoalRuntime} {
		if !strings.Contains(guide, want) {
			t.Errorf("console guide does not teach %q", want)
		}
	}
}

// Only a Claude coordinator's environment names its goal file: the hooks and
// `amux agent goal` find their state through it, and its absence is how they
// know a session is not in Claude goal mode.
func TestGoalFileEnvOnlyForClaudeCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    store.Session
		want bool
	}{
		{"claude coordinator", store.Session{ID: "wg1", Agent: "claude", Dir: "/s/wg1"}, true},
		{"codex coordinator", store.Session{ID: "wg2", Agent: agent.GoalRuntime, Dir: "/s/wg2"}, false},
		{"claude member", store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Dir: "/s/m1"}, false},
	} {
		got := envOf(AgentEnv(tc.s), claudegoal.EnvFile)
		if (got != "") != tc.want {
			t.Errorf("%s: %s=%q, want set=%v", tc.name, claudegoal.EnvFile, got, tc.want)
		}
		if tc.want && got != claudegoal.StatePath(tc.s.Dir) {
			t.Errorf("%s: goal file %q, want %q", tc.name, got, claudegoal.StatePath(tc.s.Dir))
		}
	}
}
