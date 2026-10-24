package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/centralauth"
	"amux/internal/daemon"
)

const mockSetupToken = "mock-setup-token-0123456789abcdef"

func isolateCentralCLI(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	previous := dial
	dial = func() (*daemon.Client, error) { return nil, errors.New("no daemon (test)") }
	t.Cleanup(func() { dial = previous })
}

func TestCentralClaudeLoginFromStdinStoresWithoutEchoingToken(t *testing.T) {
	isolateCentralCLI(t)
	var out bytes.Buffer
	err := claudeLogin(true, strings.NewReader(mockSetupToken+"\n"), &out)
	// The daemon is absent, so queueing a restart fails after the save.
	if err == nil || !strings.Contains(err.Error(), "login saved") {
		t.Fatalf("login = %v", err)
	}
	cred, lerr := centralauth.ClaudeStore().Load()
	if lerr != nil || cred.Token.Reveal() != mockSetupToken || cred.Generation != 1 {
		t.Fatalf("stored = %+v, %v", cred, lerr)
	}
	if strings.Contains(out.String(), mockSetupToken) || !strings.Contains(out.String(), cred.Token.Fingerprint()) {
		t.Fatalf("login output = %q", out.String())
	}

	out.Reset()
	_ = claudeLogin(true, strings.NewReader(mockSetupToken+"-reissued\n"), &out)
	if !strings.Contains(out.String(), "not revoked") || !strings.Contains(out.String(), "generation 2") {
		t.Fatalf("reissue output = %q", out.String())
	}

	status, err := captureOutput(t, func() error { return cmdAuth([]string{"status"}) })
	if err != nil || strings.Contains(status, mockSetupToken) ||
		!strings.Contains(status, "claude.auth: central (default)") || !strings.Contains(status, "generation 2") ||
		!strings.Contains(status, "one year") {
		t.Fatalf("status = %q, %v", status, err)
	}
}

func TestCentralClaudeLoginRejectsInvalidTokensWithoutSaving(t *testing.T) {
	isolateCentralCLI(t)
	for _, input := range []string{"", "\n", "two words\n", strings.Repeat("x", 5000) + "\n"} {
		var out bytes.Buffer
		if err := claudeLogin(true, strings.NewReader(input), &out); err == nil || !strings.Contains(err.Error(), "nothing was saved") {
			t.Fatalf("login(%.20q) = %v", input, err)
		}
	}
	if st, err := centralauth.ClaudeStore().Status(); err != nil || st.Configured {
		t.Fatalf("invalid input saved: %+v, %v", st, err)
	}
	status, err := captureOutput(t, func() error { return cmdAuth([]string{"status", "claude"}) })
	if err != nil || !strings.Contains(status, "run `amux auth login`") {
		t.Fatalf("status without login = %q, %v", status, err)
	}
}

func TestAuthLoginRefusesInsideASession(t *testing.T) {
	isolateCentralCLI(t)
	previous := sessionContextRestricted
	sessionContextRestricted = func() bool { return true }
	t.Cleanup(func() { sessionContextRestricted = previous })
	for _, args := range [][]string{{"login", "--token-stdin"}, {"login", "codex"}} {
		if err := cmdAuth(args); err == nil || !strings.Contains(err.Error(), "host terminal") {
			t.Fatalf("%v inside a session = %v", args, err)
		}
	}
	if st, _ := centralauth.ClaudeStore().Status(); st.Configured {
		t.Fatal("session login wrote the central store")
	}
}

func TestAuthArgumentsForHarnesses(t *testing.T) {
	isolateCentralCLI(t)
	for _, args := range [][]string{{"login", "codex", "extra"}, {"status", "codex", "x"}, {"login", "claude", "--force"}, {"status", "gemini"}} {
		if err := cmdAuth(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
