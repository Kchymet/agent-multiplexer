//go:build darwin

package panespec

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/codexapp"
	"amux/internal/codexcfg"
	"amux/internal/launchenv"
	"amux/internal/store"
)

// Optional native startup check: uses an empty configuration and no account.
func TestSeatbeltCodexStartup(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN to exercise the installed runtime")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Keep the real HOME so the production launcher resolves the installed package.
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	s := store.Session{ID: "native", RootID: "root", Agent: "codex", Dir: filepath.Join(home, "work")}
	spec := testLaunchSpec(t, s)
	ch := filepath.Join(s.Dir, ".codex")
	if err := os.MkdirAll(ch, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", ch)
	sock := appServerSocketPath(s.ID)
	argv, err := scopeSeatbelt(s.Dir, TabAgent, s, spec.Access, nil, codexcfg.NativeGoals([]string{bin, "app-server", "--listen", "unix://" + sock}), os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sup := codexapp.New(codexapp.Config{
		SessionID: s.ID, Dir: s.Dir, Endpoint: "unix://" + sock,
		Env:   append(platformLaunchEnv(spec), "CODEX_HOME="+ch),
		Goals: true, Sandbox: CodexSandboxForLaunch(),
	})
	defer sup.Close()
	if err := sup.Start(ctx, argv); err != nil {
		t.Fatal(err)
	}
	if sup.ThreadID() == "" {
		t.Fatal("handshake did not establish a thread")
	}
}

func TestSeatbeltCodexSocketIsolation(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	s := store.Session{ID: "own", Agent: "codex", Dir: filepath.Join(home, "work")}
	spec := testLaunchSpec(t, s)
	argv, err := scopeSeatbelt(s.Dir, TabAgent, s, spec.Access, nil, []string{"/bin/sh"}, home)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(appServerSocketPath(s.ID)))
	root := fmt.Sprintf("/private/tmp/codex-daemon-%d", os.Geteuid())
	own := filepath.Join(root, fmt.Sprintf("%x", sum))
	peer := own[:len(own)-1] + "p" // synthetic sibling; never use host credentials
	for _, path := range []string{own, peer} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				conn.Close()
			}
		}()
		if err := os.WriteFile(path+".lock", []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path + ".lock") })
	}
	argv = append(argv, "-c", `set -eu
/usr/bin/nc -w 1 -U "$1" </dev/null
echo own > "$1.lock"
cat "$1.lock"
if /usr/bin/nc -w 1 -U "$2" </dev/null; then exit 20; fi
if cat "$2.lock"; then exit 21; fi
if echo overwrite > "$2.lock"; then exit 22; fi
if ls "$3"; then exit 23; fi
if touch "$3/forged"; then exit 24; fi
`, "probe", own, peer, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = s.Dir
	cmd.Env, err = launchenv.Build(os.Environ(), platformLaunchEnv(spec), launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("socket isolation: %v\n%s", err, out)
	}
}

func TestSeatbeltCodexRejectsRedirectedControlDirectory(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	s := store.Session{ID: "own", Agent: "codex", Dir: filepath.Join(home, "work")}
	dir := codexcfg.AgentHome(s.Dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	peer := filepath.Join(home, "peer-control")
	if err := os.Mkdir(peer, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(peer, filepath.Join(dir, "app-server-control")); err != nil {
		t.Fatal(err)
	}
	var policy seatbeltPolicy
	if err := policy.codexSockets(s); err == nil {
		t.Fatal("session could redirect its socket grant to another control directory")
	}
}
