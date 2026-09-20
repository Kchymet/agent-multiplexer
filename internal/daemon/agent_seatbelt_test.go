//go:build darwin

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"
)

// Real CLI + Seatbelt + daemon mailbox dispatch, without a model account or a
// host daemon. This verifies that native path locators cannot grant host access
// when a child clears or forges its environment.
func TestAgentSeatbeltCommandSurface(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "amux")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", candidate, "../../cmd/amux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", "/bin/sh")
	own := store.Session{ID: "own", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "own"), ClaudeID: "11111111-1111-4111-8111-111111111111"}
	peer := store.Session{ID: "peer", RootID: "other", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "other", "peer")}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []store.Session{own, peer} {
		if err := os.MkdirAll(s.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(filepath.Join(core.StateDir(), "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	publishTestRuntime(t, d, own.ID)
	grant, err := d.authority.EnsureSession(context.Background(), own.ID, own.Dir)
	if err != nil {
		t.Fatal(err)
	}
	peerGrant, err := d.authority.EnsureSession(context.Background(), peer.ID, peer.Dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newSessionRuntime(d)
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.open(ctx, own); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	defer func() { cancel(); <-done; r.close() }()
	dir, env, argv, err := panespec.Resolve(panespec.LaunchSpec{Session: own, Access: grant}, panespec.TabTerminal)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve publishes the running test image. Replace only that fixture with
	// the compiled production CLI, at the exact path admitted by Seatbelt.
	if err := os.Rename(candidate, core.SessionBinPath(own.ID)); err != nil {
		t.Fatal(err)
	}
	script := `set -eu
amux agent name Native Name
amux agent status ready
printf '{}' | amux agent hook ready
AMUX_WORKGROUP=peer AMUX_WORKSPACE=peer amux agent name Still Own
if amux do rename peer -f name=forged; then exit 20; fi
if AMUX_SESSION_ACCESS="$1" amux agent name Stolen; then exit 21; fi
if env -u AMUX_SESSION_ACCESS -u AMUX_WORKGROUP -u AMUX_WORKSPACE amux daemon start; then exit 22; fi
if env -u AMUX_SESSION_ACCESS -u AMUX_WORKGROUP -u AMUX_WORKSPACE amux do rename peer -f name=forged; then exit 23; fi
amux agent name Verified
echo native-surface-ok
`
	argv = append(argv, "-c", script, "probe", peerGrant.CredentialHostDir)
	launchCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	cmd := exec.CommandContext(launchCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env, err = launchenv.Build(os.Environ(), env, launchenv.ForRuntime(own.Agent))
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native command surface: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "native-surface-ok") {
		t.Fatalf("unexpected output: %s", out)
	}
	got, _, err := db.GetSession(own.ID)
	if err != nil || got.Name != "Verified" {
		t.Fatalf("own session = %+v: %v", got, err)
	}
	got, _, err = db.GetSession(peer.ID)
	if err != nil || got.Name != "" {
		t.Fatalf("peer session mutated: %+v: %v", got, err)
	}
}
