package mux

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/muxclient"
)

type testPrimary struct {
	snapshot func(context.Context) ([]core.Session, error)
	dispatch func(context.Context, core.Action) (string, error)
	openPane func(context.Context, PaneRequest) (PaneRelay, error)
}

func (p testPrimary) Snapshot(ctx context.Context) ([]core.Session, error) {
	if p.snapshot == nil {
		return nil, nil
	}
	return p.snapshot(ctx)
}
func (p testPrimary) Dispatch(ctx context.Context, a core.Action) (string, error) {
	if p.dispatch == nil {
		return "", nil
	}
	return p.dispatch(ctx, a)
}
func (p testPrimary) OpenPane(ctx context.Context, request PaneRequest) (PaneRelay, error) {
	if p.openPane == nil {
		return nil, fmt.Errorf("no pane relay")
	}
	return p.openPane(ctx, request)
}

type testPaneRelay struct {
	frames chan core.PaneFrame
	input  func([]byte) error
	closed chan struct{}
	once   sync.Once
}

func newTestPaneRelay() *testPaneRelay {
	return &testPaneRelay{frames: make(chan core.PaneFrame, 16), closed: make(chan struct{})}
}

func (r *testPaneRelay) Next(ctx context.Context) (core.PaneFrame, error) {
	select {
	case frame := <-r.frames:
		return frame, nil
	case <-r.closed:
		return core.PaneFrame{}, fmt.Errorf("pane relay closed")
	case <-ctx.Done():
		return core.PaneFrame{}, ctx.Err()
	}
}
func (r *testPaneRelay) Input(data []byte) error {
	if r.input != nil {
		return r.input(data)
	}
	return nil
}
func (r *testPaneRelay) Resize(int, int) error { return nil }
func (r *testPaneRelay) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

// TestEndToEnd starts a real server on a unix socket and drives it with the real
// client: subscribe yields a snapshot, and primary-owned pane output is bridged
// back over the legacy protocol without starting a local process.
func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	sock := filepath.Join(dir, "mux.sock")
	certFile, keyFile := genCert(t, dir)
	t.Setenv("AMUX_TLS_CERT", certFile)
	t.Setenv("AMUX_TLS_KEY", keyFile)
	t.Setenv("AMUX_TLS_CA", certFile)
	t.Setenv("AMUX_TLS_SERVERNAME", "localhost")
	t.Setenv("AMUX_MUX_TOKEN", "test-mux-token")
	ln, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	relay := newTestPaneRelay()
	relay.input = func(data []byte) error {
		if strings.Contains(string(data), "MUXOKMARKER") {
			relay.frames <- core.PaneFrame{Type: core.FramePaneOutput, Data: []byte("MUXOKMARKER")}
		}
		return nil
	}
	srv := New(testPrimary{
		snapshot: func(context.Context) ([]core.Session, error) {
			return []core.Session{{ID: "console"}}, nil
		},
		openPane: func(_ context.Context, request PaneRequest) (PaneRelay, error) {
			if request.Agent != "console" || request.Tab != 2 {
				return nil, fmt.Errorf("unexpected pane request %+v", request)
			}
			return relay, nil
		},
	})
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

	if err := c.PaneOpen("p1", "console", 2 /*terminal*/, 80, 24); err != nil {
		t.Fatal(err)
	}
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

func TestNewWithoutPrimaryPaneRelayFailsClosed(t *testing.T) {
	_, err := New().primary.OpenPane(context.Background(), PaneRequest{Agent: "console"})
	if err == nil || !strings.Contains(err.Error(), "no authenticated primary pane relay") {
		t.Fatalf("default legacy mux pane relay error = %v", err)
	}
}
