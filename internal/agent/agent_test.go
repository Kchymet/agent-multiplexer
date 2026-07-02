package agent

import (
	"os"
	"path/filepath"
	"reflect"
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

// Codex Argv: the default sandbox (workspace-write) plus --model when a model is
// set, the AMUX_CODEX_BIN override, and AMUX_CODEX_SANDBOX=none omitting the
// sandbox flag entirely. PATH/SHELL are dead ends so resolve() degrades to the
// bare override name, keeping the argv comparison stable.
func TestArgvCodex(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir()) // no stray codex binary in known install spots
	t.Setenv("AMUX_CODEX_BIN", "codex-amux-test")

	cases := []struct {
		name, sandbox, model string
		want                 []string
	}{
		{"default sandbox + model", "", "gpt-5.5",
			[]string{"codex-amux-test", "--sandbox", "workspace-write", "--model", "gpt-5.5"}},
		{"explicit sandbox, no model", "read-only", "",
			[]string{"codex-amux-test", "--sandbox", "read-only"}},
		{"sandbox none omits flag", "none", "gpt-5.4",
			[]string{"codex-amux-test", "--model", "gpt-5.4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AMUX_CODEX_SANDBOX", tc.sandbox) // "" reads as unset -> the default
			argv, err := Argv("codex", tc.model)
			if err != nil {
				t.Fatalf("Argv: %v", err)
			}
			if !reflect.DeepEqual(argv, tc.want) {
				t.Fatalf("codex Argv = %v, want %v", argv, tc.want)
			}
		})
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
