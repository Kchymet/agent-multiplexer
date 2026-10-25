//go:build linux

package panespec

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/codexsocket"
	"amux/internal/store"
)

func TestScopeMapsOnlyOwnCodexPhysicalSocket(t *testing.T) {
	requireRuntimeIsolation(t)
	home, err := os.MkdirTemp("", "amux-cx-physical-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_JAIL", "on")

	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	type endpoint struct {
		session store.Session
		source  string
		target  string
		path    string
	}
	makeEndpoint := func(id string) endpoint {
		t.Helper()
		s := store.Session{ID: id, Agent: "codex", Dir: work}
		rendezvous := appServerSocketPath(id)
		if err := os.MkdirAll(filepath.Dir(rendezvous), 0o700); err != nil {
			t.Fatal(err)
		}
		source, target, err := codexsocket.PrepareNamespaceMount(rendezvous)
		if err != nil {
			t.Fatal(err)
		}
		path, err := codexsocket.ExpectedNamespacePath(rendezvous)
		if err != nil {
			t.Fatal(err)
		}
		return endpoint{session: s, source: source, target: target, path: path}
	}
	own, peer := makeEndpoint("own"), makeEndpoint("peer")
	listen := func(ep endpoint) {
		t.Helper()
		listener, err := net.Listen("unix", filepath.Join(ep.source, filepath.Base(ep.path)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
	}
	listen(own)
	listen(peer)

	alias := filepath.Join(own.source, "peer-alias")
	peerHostPath := filepath.Join(peer.source, filepath.Base(peer.path))
	if err := os.Symlink(peerHostPath, alias); err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := func(path string, deny bool) *exec.Cmd {
		t.Helper()
		spec := testLaunchSpec(t, own.session)
		argv, err := scope(work, TabAgent, own.session, spec.Access, nil, []string{exe, "-test.run=^TestSocketScopeProbe$", "-test.v"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), "PANESPEC_TEST_SOCKET_PATH="+path)
		if deny {
			cmd.Env = append(cmd.Env, "PANESPEC_TEST_SOCKET_DENY=1")
		}
		return cmd
	}

	cases := []struct {
		name string
		path string
		deny bool
	}{
		{"own-physical", own.path, false},
		{"sibling-hash", peer.path, true},
		{"host-sibling-source", peerHostPath, true},
		{"symlink-to-host-sibling", filepath.Join(own.target, filepath.Base(alias)), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if output, err := command(tc.path, tc.deny).CombinedOutput(); err != nil {
				t.Fatalf("physical socket scope probe: %v\n%s", err, output)
			}
		})
	}
}
