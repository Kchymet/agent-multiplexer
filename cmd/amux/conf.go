package main

import (
	_ "embed"
	"os"
	"path/filepath"

	"amux/internal/claudecfg"
	"amux/internal/console"
	"amux/internal/core"
)

// embeddedConf is the canonical isolated-server config, baked into the binary
// so `amux up` is self-sufficient even before install.
//
//go:embed amux.conf
var embeddedConf []byte

// ensureConf writes the isolated tmux config to ~/.config/amux/amux.conf.
// When force is false it only writes if the file is missing, so user edits are
// preserved; `amux init` passes force=true to overwrite.
func ensureConf(force bool) error {
	path := core.TmuxConfPath()
	if !force {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Make sure the control console's directory + CLAUDE.md exist too.
	_ = console.Ensure()
	return os.WriteFile(path, embeddedConf, 0o644)
}

// ensureHooks installs Claude Code's status hooks into the user's settings once.
// It's a one-time setup step (recorded by a marker), not an every-launch write:
// the hooks are a user-level setting amux configures during setup and then
// leaves alone. `amux init` passes force=true to (re)install on demand. Best
// effort — on any failure status simply falls back to "unknown".
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
