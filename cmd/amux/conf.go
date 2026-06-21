package main

import (
	_ "embed"
	"os"
	"path/filepath"

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
	return os.WriteFile(path, embeddedConf, 0o644)
}
