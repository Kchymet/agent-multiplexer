// Package agent is amux's per-kind agent registry. One Harness value owns every
// decision that varies by agent kind (claude/codex/hermes): how to launch it
// (Argv), its model catalog, its resume-vs-fresh launch plan, its trust/hook
// side effects, its sandbox config binds, its activity/turn-state read, and its
// on-disk session listing. HarnessFor is the ONE switch on kind — every other
// package asks the registry instead of branching on the string itself, so adding
// a kind is a single new harness file, not a change scattered across nine
// packages. See harness.go for the interface and the registry.
package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Argv returns the absolute argv to run for kind (with an optional model
// override), plus any extra trailing args. It delegates to the kind's Harness —
// HarnessFor is the only place kind is switched on.
//
// The launch binary is overridable per kind via AMUX_<KIND>_BIN (e.g.
// AMUX_CLAUDE_BIN). amux stays a thin launcher: point that at your own wrapper
// to own autonomy (e.g. branch on $AMUX_MODE — task vs interactive — to add
// --permission-mode acceptEdits for a hands-off task). amux never injects those
// itself.
func Argv(kind, model string, extra ...string) ([]string, error) {
	return HarnessFor(kind).Argv(model, extra...)
}

// finishArgv resolves bin to an absolute path and appends the harness args and
// caller's trailing args. Shared by every harness's Argv so path resolution and
// arg ordering stay identical across kinds.
func finishArgv(bin string, args, extra []string) []string {
	return append(append([]string{resolve(bin)}, args...), extra...)
}

// resolve returns the best command to launch bin with. It tries, in order: the
// resolver's own PATH, the user's login shell, and well-known install locations.
// The last step matters because amux execs the agent directly (the child only
// gets amux's environment) and node version managers like nvm deliberately stay
// OFF the default PATH for fast shell startup — so a lazy-loaded `claude` won't
// surface via PATH or a login shell. If everything misses it DEGRADES TO THE
// BARE NAME rather than failing. Never errors.
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
	if p := searchKnownLocations(bin); p != "" {
		return p
	}
	return bin // last resort — let the launch environment resolve it
}

// searchKnownLocations looks for bin in the common per-user install spots that
// version managers keep off the default PATH. For nvm it prefers the newest node
// version dir (which holds the current, self-contained claude binary).
func searchKnownLocations(bin string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".claude", "local", bin), // Claude Code's local install
		filepath.Join(home, ".local", "bin", bin),
		filepath.Join(home, ".bun", "bin", bin),
	}
	// nvm: ~/.nvm/versions/node/<ver>/bin/<bin>, newest version last.
	if vers, _ := filepath.Glob(filepath.Join(home, ".nvm", "versions", "node", "*", "bin", bin)); len(vers) > 0 {
		sort.Sort(byNodeVersion(vers))
		candidates = append(candidates, vers[len(vers)-1])
	}
	for _, c := range candidates {
		if isExecutable(c) {
			return c
		}
	}
	return ""
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p) // follows symlinks (nvm bins are symlinks)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// byNodeVersion orders ~/.nvm/.../node/vX.Y.Z/bin/<bin> paths by numeric
// semver so v24 sorts after v9 (plain lexical order would invert them).
type byNodeVersion []string

func (s byNodeVersion) Len() int      { return len(s) }
func (s byNodeVersion) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s byNodeVersion) Less(i, j int) bool {
	return lessVersion(nodeVersionOf(s[i]), nodeVersionOf(s[j]))
}

// nodeVersionOf pulls the "vX.Y.Z" element out of an nvm path.
func nodeVersionOf(p string) string {
	for _, part := range strings.Split(p, string(os.PathSeparator)) {
		if strings.HasPrefix(part, "v") && strings.ContainsRune(part, '.') {
			return strings.TrimPrefix(part, "v")
		}
	}
	return ""
}

// lessVersion compares dotted numeric versions component-by-component.
func lessVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an = atoi(as[i])
		}
		if i < len(bs) {
			bn = atoi(bs[i])
		}
		if an != bn {
			return an < bn
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
