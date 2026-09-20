//go:build darwin

package panespec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/cfghome"
	"amux/internal/codexcfg"
	"amux/internal/launchenv"
	"amux/internal/store"
	"amux/internal/wsops"
)

// Codex's PTY and App Server use the same Config, environment and scope. Verify
// the host file-backed login remains usable without any Claude broker grant.
func TestSeatbeltRuntimeCodexHostAuth(t *testing.T) {
	if os.Getenv("AMUX_TEST_HOST_AUTH") != "1" {
		t.Skip("set AMUX_TEST_HOST_AUTH=1 to verify Codex host auth")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	host := codexcfg.UserHome().Dir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	want, err := exec.CommandContext(ctx, bin, "login", "status").CombinedOutput()
	if err != nil || !strings.Contains(string(want), "Logged in") {
		t.Fatal("host Codex is not logged in")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", host)
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "codex-auth", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	if _, err := cfghome.Seed(codexcfg.Template(s.ID, s.Dir)); err != nil {
		t.Fatal(err)
	}
	argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{bin, "login", "status"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build(os.Environ(), append(wsops.AgentEnv(s), platformLaunchEnv(spec)...), launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cmd.CombinedOutput()
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() == 126 {
		t.Logf("launcher failed: %s", got)
	}
	// Native sandbox diagnostics may precede the status; never print login data.
	if err != nil || !strings.Contains(string(got), "Logged in using ChatGPT") {
		t.Fatalf("sandbox Codex login: error=%v loggedIn=%v notLoggedIn=%v", err, strings.Contains(string(got), "Logged in"), strings.Contains(string(got), "Not logged in"))
	}
}
