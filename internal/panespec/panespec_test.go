package panespec

import (
	"testing"

	"amux/internal/core"
)

// hasBind reports whether binds contains an entry mounting src (its second
// element) — the bwrap flag (--bind / --ro-bind-try / …) is ignored.
func hasBind(binds [][]string, src string) bool {
	for _, b := range binds {
		if len(b) >= 2 && b[1] == src {
			return true
		}
	}
	return false
}

// The agent scope must expose the Windows drive on WSL2 so Claude's clipboard
// interop (invoking a Windows .exe to read the clipboard, e.g. pasting an
// image) can find and launch it. Without /mnt/c the read fails with "can't
// find image on clipboard". See configBinds' TabAgent case.
func TestAgentScopeBindsWindowsDriveForWSLClipboard(t *testing.T) {
	binds := configBinds(TabAgent, "claude", "/home/tester")
	if !hasBind(binds, "/mnt/c") {
		t.Errorf("TabAgent scope missing /mnt/c bind (needed for WSL clipboard interop); got %v", binds)
	}
	if !hasBind(binds, "/mnt/wsl") {
		t.Errorf("TabAgent scope missing /mnt/wsl bind; got %v", binds)
	}
}

// The terminal tab already bound /mnt/wsl (for the Docker CLI symlink); make
// sure that stays intact and unaffected by the agent-scope change.
func TestTerminalScopeStillBindsMntWsl(t *testing.T) {
	binds := configBinds(TabTerminal, "claude", "/home/tester")
	if !hasBind(binds, "/mnt/wsl") {
		t.Errorf("TabTerminal scope missing /mnt/wsl bind; got %v", binds)
	}
}

// A codex agent needs its own state home ($CODEX_HOME) bound writable so it can
// write rollout transcripts and read auth/config — and must NOT get Claude's
// config binds. The shared amux state (hook-state, transcript capture) stays
// bound for every harness.
func TestCodexAgentScopeBindsCodexHome(t *testing.T) {
	ch := t.TempDir()
	t.Setenv("CODEX_HOME", ch)
	binds := configBinds(TabAgent, "codex", "/home/tester")
	if !hasBind(binds, ch) {
		t.Errorf("codex TabAgent scope missing $CODEX_HOME bind %q; got %v", ch, binds)
	}
	if hasBind(binds, "/home/tester/.claude") {
		t.Errorf("codex TabAgent scope should not bind Claude's config dir; got %v", binds)
	}
	if !hasBind(binds, core.HookStateDir()) {
		t.Errorf("codex TabAgent scope missing shared hook-state bind; got %v", binds)
	}
}
