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
	self, err := os.Executable()
	if err != nil {
		return
	}
	// The marker records the binary path and the hook-set version, so a moved
	// binary or a new hook set (bumped HooksVersion) re-installs instead of being
	// skipped — otherwise added hooks (e.g. transcript capture) never take effect.
	want := self + "\n" + claudecfg.HooksVersion + "\n"
	if !force {
		if b, err := os.ReadFile(core.HooksInstalledPath()); err == nil && string(b) == want {
			return
		}
	}
	if err := claudecfg.InstallHooks(self); err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(core.HooksInstalledPath()), 0o755)
	_ = os.WriteFile(core.HooksInstalledPath(), []byte(want), 0o644)
}
