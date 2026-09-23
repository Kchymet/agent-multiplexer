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

// NativeGoals reports whether a session runs under native goal supervision: a
// workgroup coordinator whose harness is the goal runtime. Such a session is
// always driven through the App Server (daemon.structuredControl), whatever the
// machine-wide codex.control setting says for ordinary Codex agents, and every
// user task it receives is established as its thread goal. A coordinator on any
// other harness (an explicit choice at creation) has no native goal engine;
// creation says so rather than claiming goal mode.
func NativeGoals(s store.Session) bool {
	return s.Role() == store.RoleCoordinator && Canonical(s.Agent) == GoalRuntime
}
