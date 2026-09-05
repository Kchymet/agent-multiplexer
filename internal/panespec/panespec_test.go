package panespec

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	binds := configBinds(TabAgent, store.Session{Agent: "claude"}, "/home/tester")
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
	binds := configBinds(TabTerminal, store.Session{Agent: "claude"}, "/home/tester")
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

// The agent scope no longer mounts the user's harness config: a codex agent's
// config is a private copy inside its dir (CODEX_HOME points there), so neither
// $CODEX_HOME nor Claude's ~/.claude is bound — only the shared auth file, at its
// template path, so the copy's symlink to it resolves. The shared amux state
// (hook-state, transcript capture) stays bound for every harness.
func TestCodexAgentScopeBindsOnlySharedAuth(t *testing.T) {
	ch := t.TempDir()
	t.Setenv("CODEX_HOME", ch)
	auth := filepath.Join(ch, "auth.json")
	if err := os.WriteFile(auth, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a1", Agent: "codex", Dir: t.TempDir()}
	binds := configBinds(TabAgent, s, "/home/tester")
	if hasBind(binds, ch) {
		t.Errorf("codex TabAgent scope must not bind the user's $CODEX_HOME; got %v", binds)
	}
	if !hasBind(binds, auth) {
		t.Errorf("codex TabAgent scope missing the shared auth.json bind %q; got %v", auth, binds)
	}
	if hasBind(binds, "/home/tester/.claude") || hasBind(binds, "/home/tester/.claude.json") {
		t.Errorf("codex TabAgent scope should not bind Claude's config; got %v", binds)
	}
	if !hasBind(binds, core.HookStateDir()) {
		t.Errorf("codex TabAgent scope missing shared hook-state bind; got %v", binds)
	}
}

// Likewise for Claude: the user's ~/.claude and ~/.claude.json are no longer
// mounted; only .credentials.json is, at its own path.
func TestClaudeAgentScopeBindsOnlySharedAuth(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	cred := filepath.Join(cfg, ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a1", Agent: "claude", Dir: t.TempDir()}
	binds := configBinds(TabAgent, s, "/home/tester")
	if hasBind(binds, cfg) || hasBind(binds, "/home/tester/.claude") || hasBind(binds, "/home/tester/.claude.json") {
		t.Errorf("claude TabAgent scope must not bind the user's config home; got %v", binds)
	}
	if !hasBind(binds, cred) {
		t.Errorf("claude TabAgent scope missing the shared credentials bind %q; got %v", cred, binds)
	}
}

// Native Claude installs live under ~/.local, above amux's default data dir.
// Its read-only binary mount must not hide the writable worktree/git mounts.
func TestScopeGitWorkflowWithLocalBinary(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap unavailable")
	}
	probe := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--", "/bin/true")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("bwrap unavailable: %v: %s", err, out)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	repo := filepath.Join(core.ReposDir(), "repo.git")
	dir := filepath.Join(core.SessionsDir(), "agent")
	wt := filepath.Join(dir, "repo")
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(home, "seed")
	run("git", "init", seed)
	run("git", "-C", seed, "commit", "--allow-empty", "-m", "initial")
	run("git", "clone", "--bare", seed, repo)
	run("git", "--git-dir", repo, "worktree", "add", "-b", "amux/test", wt)
	run("git", "init", "--bare", remote)
	run("git", "-C", wt, "remote", "set-url", "origin", remote)
	run("git", "-C", wt, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	bin := filepath.Join(home, ".local", "bin", "test-shell")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", bin); err != nil {
		t.Fatal(err)
	}
	// An unrelated repo remains read-only even though the assigned repo is writable.
	other := filepath.Join(core.ReposDir(), "other.git")
	run("git", "init", "--bare", other)
	script := `set -eu
 cd repo
 echo change > change.txt
 git add change.txt
 git commit -m change
 git push -u origin HEAD
 git fetch origin
 test "$(git rev-parse HEAD)" = "$(git rev-parse origin/amux/test)"
 if touch "$1/should-not-write" 2>/dev/null; then exit 1; fi
 `
	argv := scope(dir, TabAgent, store.Session{}, []string{bin, "-c", script, "test", other}, []string{repo})
	run(argv...)
	out, err := exec.Command("git", "--git-dir", remote, "log", "-1", "--format=%s", "amux/test").Output()
	if err != nil || strings.TrimSpace(string(out)) != "change" {
		t.Fatalf("push not persisted: %s, %v", out, err)
	}
}
