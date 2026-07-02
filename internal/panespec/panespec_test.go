package panespec

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
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

// Resolving the editor or terminal tab must not run the agent-launch side
// effects: the codex resume decision can rewrite the pinned conversation id in
// the store, and merely viewing a non-agent tab must never do that (a transient
// rollout-discovery miss would otherwise wipe the pin).
func TestNonAgentTabsSkipLaunchSideEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex")) // empty: pinned rollout is missing
	t.Setenv("AMUX_JAIL", "off")

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pinned := "99999999-9999-4999-8999-999999999999"
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: pinned}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for _, tab := range []int{TabEditor, TabTerminal} {
		if _, _, _, err := Resolve("a", tab); err != nil {
			t.Fatalf("Resolve(tab=%d) = %v", tab, err)
		}
	}

	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, ok, _ := db.GetSession("a")
	if !ok || s.ClaudeID != pinned {
		t.Fatalf("viewing editor/terminal tabs must not touch the pinned id, got %q", s.ClaudeID)
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
