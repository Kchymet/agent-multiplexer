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
	path, err := resolve(bin)
	if err != nil {
		return nil, err
	}
	return append(append([]string{path}, args...), extra...), nil
}

// resolve finds bin's absolute path. It first tries the current PATH, then falls
// back to the user's login shell — so launches work even when the daemon's own
// PATH is minimal (e.g. nvm/asdf-managed binaries not on the daemon's PATH).
func resolve(bin string) (string, error) {
	if filepathIsExplicit(bin) {
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, nil
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	out, err := exec.Command(shell, "-lic", "command -v "+bin).Output()
	if p := strings.TrimSpace(string(out)); err == nil && p != "" {
		return p, nil
	}
	return "", fmt.Errorf("%s not found on PATH (also tried %s -lic); install it or set AMUX_<AGENT>_BIN", bin, shell)
}

func filepathIsExplicit(p string) bool {
	return strings.ContainsRune(p, '/')
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
