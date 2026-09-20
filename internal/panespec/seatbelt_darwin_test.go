//go:build darwin

package panespec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/launchenv"
	"amux/internal/store"
)

func TestSeatbeltRuntimeOwnFilesAndProtectedAuthority(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Also exercise nonstandard layouts under a broadly readable tool root.
	roots := darwinSystemRoots
	darwinSystemRoots = append(append([]string(nil), roots...), home)
	t.Cleanup(func() { darwinSystemRoots = roots })
	t.Setenv("HOME", home)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("SHELL", "/bin/sh")
	s := store.Session{ID: "own", RootID: "root", Agent: "claude", Dir: filepath.Join(home, "sessions", "own")}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := access.Open(filepath.Join(core.StateDir(), "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	g, err := a.EnsureSession(context.Background(), s.ID, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	spec := LaunchSpec{Session: s, Access: g}
	secret := filepath.Join(home, "host-secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(s.Dir, "escape")); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(core.StateDir(), "host-authority-marker")
	if err := os.WriteFile(protected, []byte("host-only"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(secret)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	script := `set -eu
echo own > own-file
test "$(cat own-file)" = own
test -r "$AMUX_SESSION_ACCESS/current"
test -r "$AMUX_SESSION_ACCESS/context.json"
if cat "$1" 2>/dev/null; then exit 20; fi
if cat escape 2>/dev/null; then exit 21; fi
if cat <&3 2>/dev/null; then exit 22; fi
if echo bad > "$AMUX_SESSION_ACCESS/current" 2>/dev/null; then exit 23; fi
if touch "$AMUX_SESSION_ACCESS/tamper" 2>/dev/null; then exit 24; fi
if cat "$2" 2>/dev/null; then exit 25; fi
touch "$3/probe"
if touch "$4/tamper" 2>/dev/null; then exit 26; fi
if ln "$1" host-link 2>/dev/null; then
  if cat host-link 2>/dev/null; then exit 27; fi
fi
if ln "$AMUX_SESSION_ACCESS/current" credential-link 2>/dev/null; then
  if echo bad > credential-link 2>/dev/null; then exit 28; fi
fi
if ls "$HOME/.local/state/amux/access/v1/credentials" >/dev/null 2>&1; then exit 29; fi
if kill -0 "$5" 2>/dev/null; then exit 30; fi
test -x "$(command -v amux)"
test -d "$TMPDIR"
echo success
`
	argv, err := scope(s.Dir, TabTerminal, s, g, nil, []string{"/bin/sh", "-c", script, "probe", secret, protected, g.RequestsHostDir, g.MailboxHostDir, strconv.Itoa(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build(os.Environ(), platformLaunchEnv(spec), launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Seatbelt runtime: %v\n%s", err, out)
	}
	if !strings.HasSuffix(string(out), "success\n") {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestSeatbeltRuntimeReadOnlyAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_CODEX_SANDBOX", "read-only")
	s := store.Session{ID: "readonly", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	config := codexcfg.AgentHome(s.Dir)
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	script := `set -eu
if echo forbidden > workspace-write; then exit 20; fi
echo allowed > "$1/state"
echo request > "$2/request"
echo scratch > "$TMPDIR/scratch"
`
	argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{"/bin/sh", "-c", script, "probe", config, spec.Access.RequestsHostDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build(os.Environ(), platformLaunchEnv(spec), launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("read-only agent: %v: %s", err, out)
	}
}
