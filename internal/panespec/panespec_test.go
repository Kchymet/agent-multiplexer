package panespec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/git"
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

// Every tab is callable through the agent-facing pane API. None may silently
// acquire host operator capabilities such as Windows drives, Docker, or human
// shell history merely by selecting a different tab number.
func TestTabsDoNotAcquireHostOperatorCapabilities(t *testing.T) {
	for _, tab := range []int{TabAgent, TabEditor, TabTerminal} {
		binds := configBinds(tab, store.Session{Agent: "claude"}, "/home/tester")
		for _, denied := range []string{
			"/mnt/c", "/mnt/wsl", "/run/docker.sock",
			"/home/tester/.zsh_history", "/home/tester/.bash_history",
		} {
			if hasBind(binds, denied) {
				t.Errorf("tab %d acquired host capability %q: %v", tab, denied, binds)
			}
		}
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
	useFakeSecureBwrap(t)

	dir := filepath.Join(core.SessionsDir(), "r", "agent")
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

	spec := testLaunchSpec(t, store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: pinned})
	for _, tab := range []int{TabEditor, TabTerminal} {
		if _, _, _, err := Resolve(spec, tab); err != nil {
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
// template path, so the copy's symlink to it resolves. Shared hook and
// transcript state must not enter the namespace.
func TestCodexAgentScopeBindsOnlySharedAuth(t *testing.T) {
	ch := t.TempDir()
	t.Setenv("CODEX_HOME", ch)
	auth := filepath.Join(ch, "auth.json")
	mcpAuth := filepath.Join(ch, ".credentials.json")
	for _, path := range []string{auth, mcpAuth} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := store.Session{ID: "a1", Agent: "codex", Dir: t.TempDir()}
	binds := configBinds(TabAgent, s, "/home/tester")
	if hasBind(binds, ch) {
		t.Errorf("codex TabAgent scope must not bind the user's $CODEX_HOME; got %v", binds)
	}
	if !hasBind(binds, auth) {
		t.Errorf("codex TabAgent scope missing the shared auth.json bind %q; got %v", auth, binds)
	}
	if !hasBind(binds, mcpAuth) {
		t.Errorf("codex TabAgent scope missing shared MCP credentials bind %q; got %v", mcpAuth, binds)
	}
	if hasBind(binds, "/home/tester/.claude") || hasBind(binds, "/home/tester/.claude.json") {
		t.Errorf("codex TabAgent scope should not bind Claude's config; got %v", binds)
	}
	if hasBind(binds, core.HookStateDir()) || hasBind(binds, core.TranscriptDir()) {
		t.Errorf("codex TabAgent scope exposes shared hook/transcript state; got %v", binds)
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

func TestLaunchSpecRejectsBroadOrAliasedGitObjectMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	useFakeSecureBwrap(t)
	s := store.Session{ID: "a", Agent: "codex", Dir: filepath.Join(home, "agent")}
	spec := testLaunchSpec(t, s)
	pool := filepath.Join(t.TempDir(), "repo", "generation")
	objects := filepath.Join(pool, "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := git.GitObjectMount{RepoKey: "repo-key", Generation: "generation", ObjectsHostDir: objects, ObjectsMountDir: objects}
	spec.GitObjects = []git.GitObjectMount{valid}
	if _, err := validateLaunchSpec(spec); err != nil {
		t.Fatalf("valid exact object grant: %v", err)
	}

	alias := filepath.Join(t.TempDir(), "objects")
	if err := os.Symlink(objects, alias); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*git.GitObjectMount){
		"different destination": func(m *git.GitObjectMount) { m.ObjectsMountDir += "-other" },
		"pool parent":           func(m *git.GitObjectMount) { m.ObjectsHostDir, m.ObjectsMountDir = pool, pool },
		"source alias":          func(m *git.GitObjectMount) { m.ObjectsHostDir, m.ObjectsMountDir = alias, alias },
		"unsafe repository key": func(m *git.GitObjectMount) { m.RepoKey = "../peer" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := valid
			mutate(&bad)
			spec.GitObjects = []git.GitObjectMount{bad}
			if _, err := validateLaunchSpec(spec); err == nil || !strings.Contains(err.Error(), "Git object grant") {
				t.Fatalf("invalid object grant error = %v", err)
			}
		})
	}
}

// TestScopeReaches pins the visibility rule doctor uses to tell a $BROWSER (or
// any third tool) that is hidden by the scope's tmpfs $HOME apart from one that
// is missing outright: system roots and the amux data
// tree are visible; the rest of $HOME and unbound trees like /snap are not.
func TestScopeReaches(t *testing.T) {
	const data = "/home/tester/.local/share/amux"
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/usr/bin/wslview", true},
		{"/usr/bin/xdg-open", true},
		{"/home/linuxbrew/.linuxbrew/bin/browser", true},
		{"/opt/google/chrome/chrome", true},
		{"/mnt/c/Program Files/Google/Chrome/Application/chrome.exe", false},
		{"/mnt/wsl/helper", false},
		{data + "/bin/amux", false},
		{"/home/tester/.local/bin/open-browser", false}, // $HOME is a tmpfs inside the scope
		{"/home/tester/bin/firefox", false},
		{"/snap/bin/firefox", false}, // /snap is not bound
		{"/var/lib/flatpak/exports/bin/org.mozilla.firefox", false},
		{"/usrx/bin/nope", false}, // prefix match must be on a path boundary
		{"/home/linuxbrewer/x", false},
	} {
		if got := ScopeReaches(data, tc.path); got != tc.want {
			t.Errorf("ScopeReaches(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestScopeRootsMatchBinds keeps systemRoots honest: scope must bind exactly
// the roots ScopeReaches promises are visible, or doctor's verdict drifts from
// what a pane actually sees.
func TestScopeRootsMatchBinds(t *testing.T) {
	useFakeSecureBwrap(t)
	s := store.Session{ID: "a", Agent: "claude", Dir: t.TempDir()}
	spec := testLaunchSpec(t, s)
	args, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{"/usr/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, r := range systemRoots {
		if !strings.Contains(joined, " "+r+" "+r+" ") {
			t.Errorf("scope does not bind %s, but ScopeReaches reports it visible:\n%s", r, joined)
		}
	}
}
