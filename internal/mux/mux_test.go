package mux

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/muxclient"
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
	go func() { _ = New().Serve(ctx, ln) }()

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
