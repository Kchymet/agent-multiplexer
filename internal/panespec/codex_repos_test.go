package panespec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

func TestCodexLaunchDoesNotGrantSharedGitStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CODEX_BIN", "/bin/true")
	t.Setenv("AMUX_CLAUDE_BIN", "/bin/true")
	useFakeSecureBwrap(t)
	dir := filepath.Join(core.SessionsDir(), "group", "agent")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	roots := []string{filepath.Join(home, `repo with "quotes".git`), filepath.Join(home, "second.git")}
	for i, name := range []string{"one", "two", "unassigned"} {
		path := filepath.Join(home, "unassigned.git")
		if i < len(roots) {
			path = roots[i]
		}
		if err := db.PutRepo(store.Repo{Name: name, GitDir: path}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(dir, name, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s := store.Session{ID: "a", RootID: "group", Agent: "codex", Dir: dir, Repo: "one,two"}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	check := func(argv []string, want []string) {
		t.Helper()
		var got []string
		for _, arg := range argv {
			if value, ok := strings.CutPrefix(arg, "sandbox_workspace_write.writable_roots="); ok {
				if err := json.Unmarshal([]byte(value), &got); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("writable Git stores = %v, want %v; argv=%v", got, want, argv)
		}
	}
	checkFullscreen := func(argv []string, want bool) {
		t.Helper()
		got := slices.Contains(argv, `tui.alternate_screen="always"`)
		if got != want {
			t.Fatalf("alternate-screen override present = %v, want %v; argv=%v", got, want, argv)
		}
	}
	spec := testLaunchSpec(t, s)
	_, _, argv, err := Resolve(spec, TabAgent)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	checkFullscreen(argv, true)
	for _, setting := range []string{`approval_policy="on-request"`, `approvals_reviewer="auto_review"`} {
		if !slices.Contains(argv, setting) {
			t.Fatalf("terminal launch missing %s: %v", setting, argv)
		}
	}
	_, _, argv, _, err = AppServerCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	checkFullscreen(argv, false) // the background server has no TUI
	_, _, argv, err = AttachCommand(spec, "unix:///tmp/codex.sock", "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	checkFullscreen(argv, true)
	for _, setting := range []string{`approval_policy="on-request"`, `approvals_reviewer="auto_review"`} {
		if !slices.Contains(argv, setting) {
			t.Fatalf("native attach missing %s: %v", setting, argv)
		}
	}
	// A coordinator may carry repo names, but owns no worktrees or writable clones.
	s.RootID = ""
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	spec = testLaunchSpec(t, s)
	_, _, argv, _, err = AppServerCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	// Claude and shell argv must not receive Codex-specific configuration flags.
	s.RootID, s.Agent = "group", "claude"
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	spec = testLaunchSpec(t, s)
	for _, tab := range []int{TabAgent, TabTerminal} {
		_, _, argv, err = Resolve(spec, tab)
		if err != nil {
			t.Fatal(err)
		}
		check(argv, nil)
		checkFullscreen(argv, false)
	}
}

func TestEndpointRefusesLegacySharedGitLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	dir := filepath.Join(core.SessionsDir(), "root", "agent")
	if err := os.MkdirAll(filepath.Join(dir, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repo", ".git"), []byte("gitdir: /shared/cache/worktrees/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "legacy", RootID: "root", Agent: "codex", Dir: dir, Repo: "repo"}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := AppServerEndpoint(s.ID); err == nil || !strings.Contains(err.Error(), "launch refused") {
		t.Fatalf("AppServerEndpoint legacy linked worktree error = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "repo", ".git")); err != nil || !strings.Contains(string(b), "/shared/cache") {
		t.Fatalf("refusal mutated legacy checkout: %q, %v", b, err)
	}
}
