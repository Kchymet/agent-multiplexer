package main

import (
	"os"
	"path/filepath"

	"amux/internal/claudecfg"
	"amux/internal/core"
)

// ensureHooks installs Claude Code's status hooks into the user's settings once.
// It's a one-time setup step (recorded by a marker), not an every-launch write:
// the hooks are a user-level setting amux configures during setup and then
// leaves alone. Best effort — on any failure status simply falls back to
// "unknown".
func ensureHooks(force bool) {
	if !force {
		if _, err := os.Stat(core.HooksInstalledPath()); err == nil {
			return
		}
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	if err := claudecfg.InstallHooks(self); err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(core.HooksInstalledPath()), 0o755)
	_ = os.WriteFile(core.HooksInstalledPath(), []byte(self+"\n"), 0o644)
}
