package main

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/claudecfg"
)

func TestClaudeAuthCommandRoutesLoginAndStatusToSharedStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "old-private-home"))
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/wrong/account")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "mock-stale-token")
	t.Setenv("ANTHROPIC_API_KEY", "mock-key")
	bin := filepath.Join(home, "claude")
	// Fail if auth gets agent flags, a different cwd/store, or inherited tokens.
	script := `#!/bin/sh
set -eu
test "$#" = 2
test "$1" = auth
test "$PWD" = "$CLAUDE_CONFIG_DIR"
test "$PWD" = "$CLAUDE_SECURESTORAGE_CONFIG_DIR"
test -z "${CLAUDE_CODE_OAUTH_TOKEN-}"
test -z "${ANTHROPIC_API_KEY-}"
case "$2" in
  login) printf 'mock credential' > .credentials.json ;;
  status) test -f .credentials.json ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", bin)
	if err := claudecfg.Login(func() error { return runClaudeAuth("login") }); err != nil {
		t.Fatal(err)
	}
	if !claudecfg.SharedAuthEnabled() {
		t.Fatal("successful login did not enable shared store")
	}
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
