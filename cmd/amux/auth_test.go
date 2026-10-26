package main

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/amuxcfg"
)

// A saved retired claude.auth value never makes status inspect another
// store: without a daemon, status describes only the central login.
func TestClaudeAuthStatusIgnoresRetiredLegacySetting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AMUX_SOCK", filepath.Join(home, "no-daemon.sock"))
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthShared); err != nil {
		t.Fatal(err)
	}
	// A legacy status run would execute this and fail the test.
	bin := filepath.Join(home, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", bin)
	if err := cmdAuth([]string{"status"}); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeAuthRejectsInvalidArgumentsWithoutLogin(t *testing.T) {
	for _, args := range [][]string{{"login", "--force"}, {"status", "extra"}, {"restart", "--wrong"}, {"logout"}} {
		if err := cmdAuth(args); err == nil {
			t.Fatalf("accepted invalid arguments %v", args)
		}
	}
}

func TestClaudeAuthStatusInHostModeAsksTheHostAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "host-config"))
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "")
	t.Setenv("ANTHROPIC_API_KEY", "mock-host-key")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthHost); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(home, "claude")
	script := `#!/bin/sh
set -eu
test "$#" = 2
test "$1" = auth
test "$2" = status
test "$CLAUDE_CONFIG_DIR" = "$HOME/host-config"
test "${CLAUDE_SECURESTORAGE_CONFIG_DIR+set}" = set
test -z "$CLAUDE_SECURESTORAGE_CONFIG_DIR"
test "$ANTHROPIC_API_KEY" = mock-host-key
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", bin)
	if err := cmdAuth([]string{"status"}); err != nil {
		t.Fatal(err)
	}
}
