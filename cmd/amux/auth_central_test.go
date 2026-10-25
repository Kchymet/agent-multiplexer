package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/centralauth"
	"amux/internal/daemon"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

const mockSetupToken = "mock-setup-token-0123456789abcdef"

func isolateCentralCLI(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	// Never reach a developer's real daemon socket.
	t.Setenv("AMUX_SOCK", filepath.Join(home, "absent.sock"))
	previous := dial
	dial = func() (*daemon.Client, error) { return nil, errors.New("no daemon (test)") }
	t.Cleanup(func() { dial = previous })
}

type mockAccountService struct{ requests []hp.AccountRequest }

func (s *mockAccountService) Accounts(r hp.AccountRequest, ephemeral bool) (hp.AccountResult, error) {
	if !ephemeral || !hp.ValidateAccountRequest(r, ephemeral) {
		return hp.AccountResult{}, errors.New("invalid transient request")
	}
	s.requests = append(s.requests, r)
	return hp.AccountResult{OK: true}, nil
}
func TestCentralClaudeCompletionUsesProtectedDaemonTransaction(t *testing.T) {
	isolateCentralCLI(t)
	svc := &mockAccountService{}
	r := hp.AccountRequest{ProfileID: "claude:central", AttemptID: "attempt", Initiator: "host-cli"}
	if err := completeClaudeToken(svc, r, mockSetupToken+"\n"); err != nil {
		t.Fatal(err)
	}
	if len(svc.requests) != 1 || svc.requests[0].Operation != hp.AccountComplete || svc.requests[0].Secret != mockSetupToken {
		t.Fatal("missing protected completion")
	}
	if st, _ := centralauth.ClaudeStore().Status(); st.Configured {
		t.Fatal("CLI wrote credential outside daemon transaction")
	}
}
func TestCentralClaudeLoginRejectsInvalidTokensWithoutSaving(t *testing.T) {
	isolateCentralCLI(t)
	svc := &mockAccountService{}
	for _, raw := range []string{"", "\n", "two words\n", strings.Repeat("x", 5000)} {
		if err := completeClaudeToken(svc, hp.AccountRequest{}, raw); err == nil || err.Error() != "account_secret_invalid" {
			t.Fatal(err)
		}
	}
	if len(svc.requests) != 0 {
		t.Fatal("invalid secret sent to daemon")
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

func TestCentralClaudeLoginRequiresAuthorizedDaemonBeforeTokenAcquisition(t *testing.T) {
	isolateCentralCLI(t)
	_, err := accountClient()
	if err == nil {
		t.Fatal("unreachable daemon accepted")
	}
	if st, _ := centralauth.ClaudeStore().Status(); st.Configured {
		t.Fatal("offline login persisted secret")
	}
}
