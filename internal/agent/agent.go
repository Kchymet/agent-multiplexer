// Package agent resolves the command to launch for an agent kind. Resolving to
// an absolute path (against the resolver's PATH) means a spawned tmux window
// gets a valid binary even if the tmux server's own PATH is minimal.
package agent

import (
	"fmt"
	"os"
	"os/exec"
)

// Argv returns the absolute argv to run for kind, with any extra trailing args.
//
// The launch binary is overridable per kind via AMUX_<KIND>_BIN (e.g.
// AMUX_CLAUDE_BIN). amux stays a thin launcher: point that at your own wrapper
// to own autonomy (e.g. branch on $AMUX_MODE to add --permission-mode acceptEdits
// or start a /loop). amux never injects those itself.
func Argv(kind string, extra ...string) ([]string, error) {
	var bin string
	var args []string
	switch kind {
	case "", "claude":
		bin = envOr("AMUX_CLAUDE_BIN", "claude")
	case "hermes":
		bin = envOr("AMUX_HERMES_BIN", "hermes")
		args = []string{"chat"}
	default:
		return nil, fmt.Errorf("unknown agent kind %q", kind)
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%s not found on PATH — launch amux from a shell where %q works", bin, bin)
	}
	return append(append([]string{path}, args...), extra...), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
