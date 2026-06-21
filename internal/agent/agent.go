// Package agent resolves the command to launch for an agent kind. Resolving to
// an absolute path (against the resolver's PATH) means a spawned tmux window
// gets a valid binary even if the tmux server's own PATH is minimal.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Argv returns the absolute argv to run for kind (with an optional model
// override), plus any extra trailing args.
//
// The launch binary is overridable per kind via AMUX_<KIND>_BIN (e.g.
// AMUX_CLAUDE_BIN). amux stays a thin launcher: point that at your own wrapper
// to own autonomy (e.g. branch on $AMUX_MODE to add --permission-mode acceptEdits
// or start a /loop). amux never injects those itself.
func Argv(kind, model string, extra ...string) ([]string, error) {
	var bin string
	var args []string
	switch kind {
	case "", "claude":
		bin = envOr("AMUX_CLAUDE_BIN", "claude")
		// Default to the safe auto-accept permission mode (a classifier blocks
		// escalations — this is NOT --dangerously-skip-permissions). Override
		// with AMUX_PERMISSION_MODE=default|acceptEdits|plan|… or "none" to omit.
		if pm := envOr("AMUX_PERMISSION_MODE", "auto"); pm != "" && pm != "none" {
			args = append(args, "--permission-mode", pm)
		}
		if model != "" {
			args = append(args, "--model", model)
		}
	case "hermes":
		bin = envOr("AMUX_HERMES_BIN", "hermes")
		args = []string{"chat"}
		if model != "" {
			args = append(args, "-m", model)
		}
	default:
		return nil, fmt.Errorf("unknown agent kind %q", kind)
	}
	return append(append([]string{resolve(bin)}, args...), extra...), nil
}

// resolve returns the best command to launch bin with. It tries, in order, the
// resolver's own PATH and the user's login shell; if both miss (common when the
// daemon's PATH is minimal, or for lazy-loaded nvm/asdf binaries the login shell
// won't surface), it DEGRADES TO THE BARE NAME rather than failing — the tmux
// window we hand it to inherits the *server's* environment (set at `amux up`
// from the user's terminal), which can still resolve it. Never errors.
func resolve(bin string) string {
	if strings.ContainsRune(bin, '/') {
		return bin // explicit path or relative command — pass through
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if out, err := exec.Command(shell, "-lic", "command -v "+bin).Output(); err == nil {
		if p := strings.TrimSpace(string(out)); p != "" && strings.ContainsRune(p, '/') {
			return p
		}
	}
	return bin // let the tmux window's (server) environment resolve it
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
