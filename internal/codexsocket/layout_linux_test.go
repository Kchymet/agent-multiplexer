//go:build linux

package codexsocket

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
)

func testRendezvous(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(core.DataDir(), "cx", key, rendezvousName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveDialPathPreservesDirectSocket(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := testRendezvous(t, "0123456789abcdef")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	got, err := ResolveDialPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("direct Codex <=0.154 socket resolved to %q, want %q", got, path)
	}
}

func TestResolveDialPathMapsExactCodex157Alias(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := testRendezvous(t, "0123456789abcdef")
	source, _, err := PrepareNamespaceMount(path)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectedNamespacePath(path)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(source, filepath.Base(expected))
	listener, err := net.Listen("unix", physical)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(physical, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(expected, path); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveDialPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != physical {
		t.Fatalf("resolved physical path = %q, want %q", got, physical)
	}
}

func TestResolveDialPathDeniesCrossSessionAndAliases(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	own := testRendezvous(t, "0123456789abcdef")
	peer := testRendezvous(t, "fedcba9876543210")
	ownSource, _, err := PrepareNamespaceMount(own)
	if err != nil {
		t.Fatal(err)
	}
	peerExpected, err := ExpectedNamespacePath(peer)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(ownSource, filepath.Base(peerExpected))
	listener, err := net.Listen("unix", physical)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(physical, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, link, target string
	}{
		{"sibling-hash", own, peerExpected},
		{"relative-target", own, filepath.Base(peerExpected)},
		{"malformed-key", testRendezvous(t, "short"), peerExpected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(tc.link)
			if err := os.Symlink(tc.target, tc.link); err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveDialPath(tc.link); err == nil {
				t.Fatalf("accepted unauthorized rendezvous %q -> %q", tc.link, tc.target)
			}
		})
	}

	_ = os.Remove(own)
	ownExpected, err := ExpectedNamespacePath(own)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ownExpected, own); err != nil {
		t.Fatal(err)
	}
	nonClean := filepath.Dir(own) + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(filepath.Dir(own)) + string(filepath.Separator) + rendezvousName
	if _, err := ResolveDialPath(nonClean); err == nil || !strings.Contains(err.Error(), "absolute and clean") {
		t.Fatalf("noncanonical alias error = %v", err)
	}
}

func TestResolveDialPathDeniesRedirectedPhysicalDirectoryAndEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		redirectDir bool
	}{
		{"directory-symlink", true},
		{"endpoint-symlink", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			path := testRendezvous(t, "0123456789abcdef")
			source, _, err := PrepareNamespaceMount(path)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := ExpectedNamespacePath(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(expected, path); err != nil {
				t.Fatal(err)
			}

			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			outsideSocket := filepath.Join(outside, "socket")
			listener, err := net.Listen("unix", outsideSocket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if tc.redirectDir {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, source); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outsideSocket, filepath.Join(source, filepath.Base(expected))); err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveDialPath(path); err == nil {
				t.Fatal("accepted redirected physical socket path")
			}
		})
	}
}
