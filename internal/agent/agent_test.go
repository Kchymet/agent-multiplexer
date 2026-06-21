package agent

import "testing"

// When the binary can't be resolved in the resolver's own PATH or via the login
// shell, Argv must degrade to the bare name (so the tmux window's server env can
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
