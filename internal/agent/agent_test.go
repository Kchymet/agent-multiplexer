package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// When the binary can't be resolved in the resolver's own PATH or via the login
// shell, Argv must degrade to the bare name (so the spawned pane's shell env can
// resolve it) rather than failing.
func TestArgvDegradesToBareName(t *testing.T) {
	t.Setenv("AMUX_CLAUDE_BIN", "claude-amux-does-not-exist-xyz")
	t.Setenv("AMUX_PERMISSION_MODE", "none")
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("SHELL", "/bin/sh")

	argv, err := Argv("claude", "")
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	if len(argv) == 0 || argv[0] != "claude-amux-does-not-exist-xyz" {
		t.Fatalf("want bare-name degrade, got %v", argv)
	}
}

// When a binary isn't on PATH or surfaced by the login shell (e.g. nvm kept off
// PATH for fast shell startup), the resolver must still find it in a known nvm
// version dir — preferring the newest version — so amux can exec it directly.
func TestResolveFindsNvmBinaryOffPath(t *testing.T) {
	home := t.TempDir()
	for _, ver := range []string{"v9.9.9", "v10.0.0", "v24.16.0"} {
		dir := filepath.Join(home, ".nvm", "versions", "node", ver, "bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin") // nvm not on PATH
	t.Setenv("SHELL", "/bin/sh")      // a non-interactive sh won't surface it either

	got := resolve("claude")
	want := filepath.Join(home, ".nvm", "versions", "node", "v24.16.0", "bin", "claude")
	if got != want {
		t.Fatalf("resolve(claude) = %q, want newest nvm version %q", got, want)
	}
}

// An explicit path is passed through untouched.
func TestArgvAbsolutePassThrough(t *testing.T) {
	t.Setenv("AMUX_CLAUDE_BIN", "/usr/bin/env")
	t.Setenv("AMUX_PERMISSION_MODE", "none")

	argv, err := Argv("claude", "")
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	if argv[0] != "/usr/bin/env" {
		t.Fatalf("want /usr/bin/env passthrough, got %v", argv)
	}
}
