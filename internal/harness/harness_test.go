package harness

import (
	"net"
	"strings"
	"testing"
	"time"

	"amux/internal/harnessproto"
)

func TestHarnessSpawnStreamsOutputAndExit(t *testing.T) {
	a, b := net.Pipe()
	mux := harnessproto.NewConn(a)
	go func() { _ = Serve(harnessproto.NewConn(b)) }()

	if r, err := mux.ReadHarness(); err != nil || r.Type != harnessproto.HReady {
		t.Fatalf("expected ready, got %+v err=%v", r, err)
	}

	if err := mux.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: "p1",
		Argv: []string{"sh", "-c", "printf HELLO_HARNESS"}, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}

	var out []byte
	done := make(chan struct{})
	go func() {
		for {
			m, err := mux.ReadHarness()
			if err != nil {
				close(done)
				return
			}
			if m.Type == harnessproto.HOutput {
				out = append(out, m.Data...)
			}
			if m.Type == harnessproto.HExit {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pane output/exit")
	}
	if !strings.Contains(string(out), "HELLO_HARNESS") {
		t.Fatalf("output missing marker: %q", out)
	}
}
