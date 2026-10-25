package agent

import (
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// GoalRuntime is the harness kind whose runtime supervises a persistent goal
// natively: Codex, whose App Server owns the thread goal (objective, status,
// token budget and usage), continues an active goal on every idle turn by
// itself, and exposes get/update_goal to the model. It is the default
// coordinator kind (see wsops.CreateWorkspace) because a coordinator's job —
// run the user's task to verified completion with minimal intervention — is
// exactly what a native goal provides. Ordinary agents keep DefaultKind.
const GoalRuntime = harnessproto.RuntimeCodex

// HookGoalRuntime is the harness kind amux runs in goal mode through its own
// hooks rather than a native goal engine: Claude Code. A Claude coordinator's
// goal lives in a session-local state file (see internal/claudegoal); every user
// task becomes the active goal on UserPromptSubmit, and a Stop hook continues
// the turn until the model marks the goal complete, asks the user for input, or
// the user pauses it.
const HookGoalRuntime = harnessproto.RuntimeClaude

// NativeGoals reports whether a session runs under native goal supervision: a
// workgroup coordinator whose harness is the goal runtime. Such a session is
// always driven through the App Server (daemon.structuredControl), whatever the
// machine-wide codex.control setting says for ordinary Codex agents, and every
// user task it receives is established as its thread goal. A coordinator on a
// harness without a native goal engine may still run in goal mode through
// amux's hooks (HookGoals); GoalMode covers both.
func NativeGoals(s store.Session) bool {
	return s.Role() == store.RoleCoordinator && Canonical(s.Agent) == GoalRuntime
}

// HookGoals reports whether a session runs in goal mode through amux's Claude
// hooks: a workgroup coordinator on HookGoalRuntime. It is on by default for
// every such coordinator — there is no opt-in.
func HookGoals(s store.Session) bool {
	return s.Role() == store.RoleCoordinator && Canonical(s.Agent) == HookGoalRuntime
}

// GoalMode reports whether a session runs every user task as a goal, by either
// mechanism: a native Codex thread goal or amux's Claude goal hooks.
func GoalMode(s store.Session) bool { return NativeGoals(s) || HookGoals(s) }

// goalKind reports whether a coordinator of this kind runs in goal mode.
func goalKind(kind string) bool {
	k := Canonical(kind)
	return k == GoalRuntime || k == HookGoalRuntime
}
