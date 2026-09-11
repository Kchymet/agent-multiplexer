package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/access"
)

// TestSelfAgentID covers how `amux agent done` resolves the caller's own store id
// only from the fixed mounted session context. Arguments and legacy environment
// hints cannot select completion authority.
func TestSelfAgentID(t *testing.T) {
	tests := []struct {
		name string
		args []string
		ctx  access.SessionContext
		err  error
		want string
	}{
		{
			name: "fixed context resolves the id",
			ctx:  access.SessionContext{Protocol: access.ProtocolVersion, SubjectID: "wg-1", MailboxDir: "/mailbox"},
			want: "wg-1",
		},
		{
			name: "id argument cannot select authority",
			args: []string{"--id", "flag-id"},
			ctx:  access.SessionContext{Protocol: access.ProtocolVersion, SubjectID: "wg-1", MailboxDir: "/mailbox"},
		},
		{
			name: "missing fixed context",
			err:  os.ErrNotExist,
		},
		{
			name: "blank fixed subject",
			ctx:  access.SessionContext{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selfAgentID(tt.args, func() (access.SessionContext, error) { return tt.ctx, tt.err })
			if got != tt.want || (tt.want == "" && err == nil) || (tt.want != "" && err != nil) {
				t.Errorf("selfAgentID(%v) = %q, %v; want %q", tt.args, got, err, tt.want)
			}
		})
	}
}

func TestAgentDoneReturnsFailures(t *testing.T) {
	sandboxCLI(t)
	if err := cmdAgentDone(nil); err == nil || !strings.Contains(err.Error(), "not inside") {
		t.Fatalf("missing identity error = %v", err)
	}

	oldLoad := loadAgentSessionContext
	oldRestricted := sessionContextRestricted
	oldOpen := openRestrictedSessionRPC
	loadAgentSessionContext = func() (access.SessionContext, error) {
		return access.SessionContext{Protocol: access.ProtocolVersion, SubjectID: "agent-123", MailboxDir: "/mailbox"}, nil
	}
	sessionContextRestricted = func() bool { return true }
	openRestrictedSessionRPC = func() (restrictedSessionRPC, error) { return nil, errors.New("daemon offline (test)") }
	t.Cleanup(func() {
		loadAgentSessionContext = oldLoad
		sessionContextRestricted = oldRestricted
		openRestrictedSessionRPC = oldOpen
	})
	err := cmdAgentDone(nil)
	if err == nil || !strings.Contains(err.Error(), "archive agent-123") || !strings.Contains(err.Error(), "daemon offline (test)") {
		t.Fatalf("daemon failure = %v", err)
	}
}

// TestAgentDoneCLIChild calls the real main entrypoint in a subprocess so the
// regression covers the shell-visible exit status, not only a returned error.
func TestAgentDoneCLIChild(t *testing.T) {
	if os.Getenv("AMUX_TEST_AGENT_DONE_EXIT") != "1" {
		return
	}
	os.Args = []string{"amux", "agent", "done"}
	main()
}

func TestAgentDoneFailureExitsNonzero(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAgentDoneCLIChild$")
	cmd.Env = []string{
		"AMUX_TEST_AGENT_DONE_EXIT=1",
		"AMUX_WORKGROUP=agent-123",
		"AMUX_WORKSPACE=",
		"AMUX_SOCK=" + filepath.Join(home, "absent.sock"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "run"),
	}
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("amux agent done exit = %v, output:\n%s", err, out)
	}
	if !strings.Contains(string(out), "not inside") {
		t.Fatalf("amux agent done did not require fixed session context:\n%s", out)
	}
	if strings.Contains(string(out), "daemon started") {
		t.Fatalf("amux agent done attempted a host daemon operation:\n%s", out)
	}
}
