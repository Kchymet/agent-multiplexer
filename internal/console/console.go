// Package console provides the amux control console: a built-in, always-present
// session that runs an agent in a neutral directory, preconfigured (via its
// CLAUDE.md) to help the user configure and operate amux — and scoped to config
// + CLI only, never workspace or amux source code.
package console

import (
	_ "embed"
	"os"
	"path/filepath"

	"amux/internal/core"
	"amux/internal/store"
)

// ID is the reserved workspace id for the console (never a real workspace).
const ID = "console"

//go:embed CLAUDE.md
var claudeMD []byte

// Dir is the console's neutral working directory.
func Dir() string { return core.ConsoleDir() }

// Ensure creates the console directory and writes its CLAUDE.md if missing
// (existing user edits are preserved).
func Ensure() error {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	p := filepath.Join(Dir(), "CLAUDE.md")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, claudeMD, 0o644)
}

// Workspace returns the synthetic workspace describing the console (not stored
// in the registry).
func Workspace() store.Workspace {
	return store.Workspace{
		ID:    ID,
		Name:  "amux console",
		Agent: "claude",
		Mode:  "console",
		Dir:   Dir(),
	}
}
