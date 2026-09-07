package panespec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/store"
)

func TestCodexLaunchDoesNotGrantSharedGitStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CODEX_BIN", "/bin/true")
	t.Setenv("AMUX_CLAUDE_BIN", "/bin/true")
	t.Setenv("AMUX_JAIL", "off")
	dir := filepath.Join(home, "agent")
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
	_, _, argv, err := Resolve(s.ID, TabAgent)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	_, _, argv, _, err = AppServerCommand(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	// A coordinator may carry repo names, but owns no worktrees or writable clones.
	s.RootID = ""
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	_, _, argv, _, err = AppServerCommand(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	check(argv, nil)
	// Claude and shell argv must not receive Codex-specific configuration flags.
	s.RootID, s.Agent = "group", "claude"
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	for _, tab := range []int{TabAgent, TabTerminal} {
		_, _, argv, err = Resolve(s.ID, tab)
		if err != nil {
			t.Fatal(err)
		}
		check(argv, nil)
	}
}

func TestResolveRefusesLegacySharedGitLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "off")
	dir := filepath.Join(home, "agent")
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
	if err := db.PutSession(store.Session{ID: "legacy", RootID: "root", Agent: "codex", Dir: dir, Repo: "repo"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, _, _, err := Resolve("legacy", TabAgent); err == nil || !strings.Contains(err.Error(), "launch refused") {
		t.Fatalf("Resolve legacy linked worktree error = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "repo", ".git")); err != nil || !strings.Contains(string(b), "/shared/cache") {
		t.Fatalf("refusal mutated legacy checkout: %q, %v", b, err)
	}
}
