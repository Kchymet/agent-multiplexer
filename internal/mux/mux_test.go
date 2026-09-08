package mux

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/muxclient"
	"amux/internal/panespec"
)

// TestEndToEnd starts a real server on a unix socket and drives it with the real
// client: subscribe yields a snapshot, and opening the console's terminal tab
// runs a shell whose output streams back over the protocol.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("AMUX_JAIL", "off") // a plain shell; bwrap may be unavailable in CI
	t.Setenv("SHELL", "/bin/sh")

	sock := filepath.Join(dir, "mux.sock")
	ln, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	authority, err := access.Open(filepath.Join(dir, "access"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	resolver := func(ctx context.Context, id string) (panespec.LaunchSpec, error) {
		if id != console.ID {
			return panespec.LaunchSpec{}, fmt.Errorf("unexpected session %q", id)
		}
		if err := console.Ensure(); err != nil {
			return panespec.LaunchSpec{}, err
		}
		session := console.Session()
		grant, err := authority.EnsureSession(ctx, session.ID, session.Dir)
		return panespec.LaunchSpec{Session: session, Access: grant}, err
	}
	srv := New(resolver)
	// This test exercises mux routing, not namespace construction. Namespace
	// behavior has its own tests and now correctly rejects credentials when the
	// jail is disabled.
	srv.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
		return spec.Session.Dir, nil, []string{"/bin/sh"}, nil
	}
	go func() { _ = srv.Serve(ctx, ln) }()

	var mu sync.Mutex
	var out []byte
	gotSnap := make(chan int, 8)
	gotMarker := make(chan struct{})
	var once sync.Once

	c, err := muxclient.Dial("unix:"+sock, muxclient.Handlers{
		OnSnapshot: func(s []core.Session) {
			select {
			case gotSnap <- len(s):
			default:
			}
		},
		OnPaneOutput: func(_ string, data []byte) {
			mu.Lock()
			out = append(out, data...)
			has := strings.Contains(string(out), "MUXOKMARKER")
			mu.Unlock()
			if has {
				once.Do(func() { close(gotMarker) })
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Subscribe(); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-gotSnap:
		if n < 1 {
			t.Fatalf("snapshot had %d sessions, want >=1 (the console)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot received")
	}

	if err := c.PaneOpen("p1", console.ID, 2 /*terminal*/, 80, 24); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // let the shell come up
	if err := c.PaneInput("p1", []byte("echo MUXOKMARKER\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotMarker:
	case <-time.After(10 * time.Second):
		mu.Lock()
		got := string(out)
		mu.Unlock()
		t.Fatalf("pane output never contained the marker; got %q", got)
	}
}

func TestNewWithoutLaunchSpecResolverFailsClosed(t *testing.T) {
	_, err := New().launchSpec(context.Background(), console.ID)
	if err == nil || !strings.Contains(err.Error(), "no daemon-authorized launch resolver") {
		t.Fatalf("default legacy mux resolver error = %v", err)
	}
}
