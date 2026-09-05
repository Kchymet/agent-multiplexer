// Package console provides the amux control console: a built-in, always-present
// session that runs an agent in a neutral directory with context over
// everything amux runs on this machine — every workgroup, agent, and repo — and
// operates amux for the user through its CLI. It is the machine-wide default
// session (store.RoleConsole); its guide (CLAUDE.md) is generated at every
// launch by wsops from the live inventory, like every agent's.
package console

import (
	"os"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
)

func init() {
	// The console has a private config home like any agent; register its dir so
	// harness listings (amux agent sessions) include its conversations.
	agent.RegisterConsoleDir(Dir)
}

// ID is the reserved workspace id for the console (never a real workspace).
const ID = "console"

// SessionID is the console's stable conversation id (a fixed valid UUID), so it
// always resumes the same config session across restarts.
const SessionID = "a3c00501-0000-4000-8000-0000c0501010"

// Dir is the console's neutral working directory.
func Dir() string { return core.ConsoleDir() }

// Ensure creates the console directory. Its guide is written at launch (see
// wsops.AgentCommand), and trust is granted then too, in the console's own
// config home (agent.Harness.PrepareLaunch) — the user's ~/.claude.json is never
// edited.
func Ensure() error {
	return os.MkdirAll(Dir(), 0o755)
}

// Session returns the synthetic session describing the console (not stored in
// the DB). Mode is store.ModeConsole, which is also what marks its role.
func Session() store.Session {
	return store.Session{
		ID:       ID,
		Name:     "amux console",
		Agent:    agent.DefaultKind(),
		Mode:     store.ModeConsole,
		Dir:      Dir(),
		ClaudeID: SessionID,
	}
}
