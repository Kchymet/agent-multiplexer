package panespec

import "testing"

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
	binds := configBinds(TabAgent, "/home/tester")
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
	binds := configBinds(TabTerminal, "/home/tester")
	if !hasBind(binds, "/mnt/wsl") {
		t.Errorf("TabTerminal scope missing /mnt/wsl bind; got %v", binds)
	}
}
