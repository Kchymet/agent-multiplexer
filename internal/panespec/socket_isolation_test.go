package panespec

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/store"
)

// This helper runs as a subprocess inside the exact pane namespace. Merely
// checking write permission on the parent directory does not test socket access.
func TestSocketScopeProbe(t *testing.T) {
	path := os.Getenv("AMUX_TEST_SOCKET_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	if os.Getenv("AMUX_TEST_SOCKET_WAIT") == "1" {
		fmt.Println("SCOPE_READY")
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		conn.Close()
	}
	if os.Getenv("AMUX_TEST_SOCKET_DENY") == "1" {
		if err == nil {
			t.Fatal("sandbox connected to another session's private socket")
		}
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
			t.Fatalf("socket probe failed for an unrelated reason: %v", err)
		}
	} else if err != nil {
		t.Fatalf("sandbox cannot connect to its own socket: %v", err)
	}
}

func TestScopeRejectsSiblingSessionSocket(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	home, err := os.MkdirTemp("", "cx-scope-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	realData := filepath.Join(home, ".local", "share")
	if err := os.MkdirAll(realData, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realData, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_JAIL", "on")
	own := filepath.Join(core.SessionsDir(), "wg", "own")
	sibling := filepath.Join(core.SessionsDir(), "wg", "peer")
	listen := func(sessionID string) string {
		t.Helper()
		path := appServerSocketPath(sessionID)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			for {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return path
	}
	ownSocket, peerSocket, coordinatorSocket := listen("own"), listen("peer"), listen("wg")
	for _, dir := range []string{own, sibling} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Launch from the same home subtree as the canonical data path, so that
	// broad runtime bind also exposes the alternate path unless it is masked.
	runtimeBin := filepath.Join(home, ".local", "lib", "socket-probe")
	launcher := filepath.Join(home, ".local", "bin", "socket-probe")
	for _, p := range []string{runtimeBin, launcher} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.OpenFile(runtimeBin, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := os.Symlink(runtimeBin, launcher); err != nil {
		t.Fatal(err)
	}
	exe = launcher
	canonicalPeer, err := filepath.EvalSymlinks(peerSocket)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, id, dir, path, deny string }{
		{"own", "own", own, ownSocket, "0"},
		{"sibling", "own", own, peerSocket, "1"},
		{"sibling-canonical-alias", "own", own, canonicalPeer, "1"},
		{"sibling-proc-alias", "own", own, fmt.Sprintf("/proc/%d/root%s", os.Getpid(), peerSocket), "1"},
		{"coordinator-own", "wg", filepath.Dir(own), coordinatorSocket, "0"},
		{"coordinator-peer", "wg", filepath.Dir(own), peerSocket, "1"},
	}
	command := func(t *testing.T, id, dir, path, deny string) *exec.Cmd {
		t.Helper()
		argv := scope(dir, TabAgent, store.Session{ID: id, Agent: "codex", Dir: dir}, []string{exe, "-test.run=^TestSocketScopeProbe$", "-test.v"}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "AMUX_TEST_SOCKET_PATH="+path, "AMUX_TEST_SOCKET_DENY="+deny)
		return cmd
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if output, err := command(t, tc.id, tc.dir, tc.path, tc.deny).CombinedOutput(); err != nil {
				t.Fatalf("scoped %s socket probe: %v\n%s", tc.name, err, output)
			}
		})
	}

	for _, late := range []struct{ name, session, target, deny string }{
		{"peer-created-after-pane-start", "own", "future-peer", "1"},
		{"own-created-after-pane-start", "future-own", "future-own", "0"},
	} {
		t.Run(late.name, func(t *testing.T) {
			cmd := command(t, late.session, own, appServerSocketPath(late.target), late.deny)
			cmd.Env = append(cmd.Env, "AMUX_TEST_SOCKET_WAIT=1")
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			reader := bufio.NewReader(output)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					_ = cmd.Wait()
					t.Fatalf("wait for mounted scope: %v", err)
				}
				if line == "SCOPE_READY\n" {
					break
				}
			}
			listen(late.target) // create after the child entered its mount namespace
			if _, err := io.WriteString(input, "continue\n"); err != nil {
				t.Fatal(err)
			}
			_ = input.Close()
			rest, _ := io.ReadAll(reader)
			if err := cmd.Wait(); err != nil {
				t.Fatalf("socket created after scope setup: %v\n%s", err, rest)
			}
		})
	}
}
