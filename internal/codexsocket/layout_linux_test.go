//go:build linux

package codexsocket

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestDialContextPreservesDirectSocket(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := testRendezvous(t, "0123456789abcdef")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()
	conn, err := DialContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted
}

func TestDialContextMapsExactCodex157Alias(t *testing.T) {
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

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()
	conn, err := DialContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted
}

func TestDialContextPinsSocketAcrossResolveConnectReplacement(t *testing.T) {
	for _, relocated := range []bool{false, true} {
		name := "direct"
		if relocated {
			name = "relocated"
		}
		t.Run(name, func(t *testing.T) {
			dataHome, err := os.MkdirTemp("/tmp", "cx-race-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dataHome) })
			t.Setenv("XDG_DATA_HOME", dataHome)
			rendezvous := testRendezvous(t, "0123456789abcdef")
			source := rendezvous
			if relocated {
				physicalDir, _, err := PrepareNamespaceMount(rendezvous)
				if err != nil {
					t.Fatal(err)
				}
				expected, err := ExpectedNamespacePath(rendezvous)
				if err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(physicalDir, filepath.Base(expected))
				if err := os.Symlink(expected, rendezvous); err != nil {
					t.Fatal(err)
				}
			}

			own, err := net.Listen("unix", source)
			if err != nil {
				t.Fatal(err)
			}
			defer own.Close()
			if relocated {
				if err := os.Chmod(source, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			peerPath := filepath.Join(t.TempDir(), "peer.sock")
			peer, err := net.Listen("unix", peerPath)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			serveLabel := func(listener net.Listener, label string) {
				go func() {
					conn, err := listener.Accept()
					if err == nil {
						_, _ = conn.Write([]byte(label))
						_ = conn.Close()
					}
				}()
			}
			serveLabel(own, "own")
			serveLabel(peer, "peer")

			key, owned, err := layoutKey(rendezvous)
			if err != nil || !owned {
				t.Fatalf("layout key: owned=%v err=%v", owned, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := dialManagedContext(ctx, rendezvous, key, func() {
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(peerPath, source); err != nil {
					t.Fatal(err)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			got, err := io.ReadAll(conn)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "own" {
				t.Fatalf("post-validation replacement redirected dial to %q", got)
			}
		})
	}
}

func TestDialContextPreservesCustomSymlinkEndpoint(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	alias := filepath.Join(t.TempDir(), "alias.sock")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
		close(accepted)
	}()
	conn, err := DialContext(context.Background(), alias)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	<-accepted
}

func TestDialContextDeniesCrossSessionAndAliases(t *testing.T) {
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
			if _, err := DialContext(context.Background(), tc.link); err == nil {
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
	if _, err := DialContext(context.Background(), nonClean); err == nil || !strings.Contains(err.Error(), "absolute and clean") {
		t.Fatalf("noncanonical alias error = %v", err)
	}
}

func TestDialContextDeniesRedirectedPhysicalDirectoryAndEndpoint(t *testing.T) {
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
			if _, err := DialContext(context.Background(), path); err == nil {
				t.Fatal("accepted redirected physical socket path")
			}
		})
	}
}
