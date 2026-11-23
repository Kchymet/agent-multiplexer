// Package overseer provides the overseer: a built-in, always-present session
// that supervises every workgroup coordinator, owns the global item inbox
// (items.GlobalInbox) and curates the user question queue. It is the
// coordinator of coordinators — it holds no admin powers (accounts, auth,
// direct clients, updates, repo tracking, delete, move); those stay with the
// local admin console (internal/console). Like the console it is synthetic
// (never a store row) and its guide is generated at every launch by wsops.
package overseer

import (
	"os"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
)

func init() {
	// A private config home like any agent; list its conversations too.
	agent.RegisterConsoleDir(Dir)
}

// ID is the reserved session id of the overseer (never a real workspace).
const ID = "overseer"

// SessionID is the overseer's stable conversation id (a fixed valid UUID), so
// it always resumes the same config session across restarts.
const SessionID = "a3c00501-0000-4000-8000-00000ee55e01"

// Dir is the overseer's neutral working directory.
func Dir() string { return core.OverseerDir() }

// Ensure creates the overseer directory. Like the console's, it is created at
// launch, never by a read-side lookup.
func Ensure() error { return os.MkdirAll(Dir(), 0o755) }

// Session returns the synthetic session describing the overseer (not stored in
// the DB). Mode is store.ModeOverseer, which is also what marks its role.
func Session() store.Session {
	return store.Session{
		ID:       ID,
		Name:     "amux overseer",
		Agent:    agent.DefaultKind(),
		Mode:     store.ModeOverseer,
		Dir:      Dir(),
		ClaudeID: SessionID,
	}
}

// IsID reports whether id names the overseer.
func IsID(id string) bool { return id == ID }
