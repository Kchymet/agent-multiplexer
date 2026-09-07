package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfAgentID covers how `amux agent done` resolves the caller's own store id
// without being handed one: an explicit --id flag wins, then $AMUX_WORKGROUP,
// then its legacy $AMUX_WORKSPACE alias, and it's empty when nothing identifies
// the agent (so the verb no-ops rather than archiving the wrong session).
func TestSelfAgentID(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "AMUX_WORKGROUP resolves the id",
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1"},
			want: "wg-1",
		},
		{
			name: "AMUX_WORKSPACE is the fallback alias",
			env:  map[string]string{"AMUX_WORKSPACE": "wg-2"},
			want: "wg-2",
		},
		{
			name: "AMUX_WORKGROUP wins over the alias",
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1", "AMUX_WORKSPACE": "wg-2"},
			want: "wg-1",
		},
		{
			name: "--id flag overrides the environment",
			args: []string{"--id", "flag-id"},
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1"},
			want: "flag-id",
		},
		{
			name: "--id=value form",
			args: []string{"--id=flag-id"},
			want: "flag-id",
		},
		{
			name: "no id anywhere is empty (no-op)",
			env:  map[string]string{},
			want: "",
		},
		{
			name: "blank env var is treated as unset",
			env:  map[string]string{"AMUX_WORKGROUP": "  "},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selfAgentID(tt.args, env(tt.env)); got != tt.want {
				t.Errorf("selfAgentID(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestAgentDoneReturnsFailures(t *testing.T) {
	sandboxCLI(t)
	if err := cmdAgentDone(nil); err == nil || !strings.Contains(err.Error(), "$AMUX_WORKGROUP unset") {
		t.Fatalf("missing identity error = %v", err)
	}

	t.Setenv("AMUX_WORKGROUP", "agent-123")
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
	if !strings.Contains(string(out), "archive agent-123") {
		t.Fatalf("amux agent done output lost target identity:\n%s", out)
	}
	if !strings.Contains(string(out), "not starting") && !strings.Contains(string(out), "refusing to start") {
		t.Fatalf("amux agent done output did not explain startup refusal:\n%s", out)
	}
}
