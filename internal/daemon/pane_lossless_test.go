package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"amux/internal/core"
)

// TestPaneOutputLossless is the ghost-text regression. Pane output is a stateful
// terminal byte stream: dropping any of it (e.g. an erase sequence) corrupts the
// client's emulator and leaves stale text ghosting. A slow client must therefore
// receive every byte, in order — the daemon coalesces into its per-pane buffer
// rather than dropping. net.Pipe is synchronous, so the reader below is as slow
// as it gets; none of the stream may be lost.
func TestPaneOutputLossless(t *testing.T) {
	srv, cli := net.Pipe()
	cl := newConnState(srv)
	defer cl.stop()
	defer cli.Close()

	const chunks = 4000
	var want bytes.Buffer
	for i := 0; i < chunks; i++ {
		want.WriteString(fmt.Sprintf("<%d>", i))
	}
	go func() {
		for i := 0; i < chunks; i++ {
			cl.paneOutput("p", []byte(fmt.Sprintf("<%d>", i)))
		}
	}()

	dec := json.NewDecoder(cli)
	var got bytes.Buffer
	_ = cli.SetReadDeadline(time.Now().Add(5 * time.Second))
	for got.Len() < want.Len() {
		var f core.PaneFrame
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("decode after %d/%d bytes: %v", got.Len(), want.Len(), err)
		}
		switch f.Type {
		case core.FramePaneReset:
			t.Fatalf("unexpected resync for a %d-byte stream (well under the cap)", want.Len())
		case core.FramePaneOutput:
			got.Write(f.Data)
		}
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("stream corrupted: got %d bytes, want %d (first mismatch reveals a drop/reorder)", got.Len(), want.Len())
	}
}

// TestPaneOutputResyncOnOverflow covers the safety valve: a client so far behind
// that the backlog passes paneOutCap. Dropping bytes from the middle would
// corrupt the emulator, so instead the buffer keeps only the recent tail and
// flags a reset — the client clears its screen, and the agent's next repaint
// (present in the retained tail) rebuilds it. Driven directly (no writeLoop) so
// the overflow is deterministic.
func TestPaneOutputResyncOnOverflow(t *testing.T) {
	cl := &connState{
		obuf: map[string]*paneOut{},
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}

	// Feed 64 KiB chunks (nothing drains) until the backlog first passes the cap
	// and trims. At that instant the buffer holds exactly the retained tail.
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for i := 0; ; i++ {
		cl.paneOutput("p", chunk)
		if cl.obuf["p"].reset {
			break
		}
		if i > (paneOutCap/len(chunk))*2 {
			t.Fatal("backlog never triggered a resync past paneOutCap")
		}
	}
	if got := len(cl.obuf["p"].data); got != paneOutKeep {
		t.Fatalf("on resync kept %d bytes, want the paneOutKeep tail (%d)", got, paneOutKeep)
	}

	// Further output after the trim accumulates on top of the tail (still lossless
	// from the reset point), bounded by the cap.
	tail := bytes.Repeat([]byte("E"), 32<<10)
	cl.paneOutput("p", tail)
	b := cl.obuf["p"]
	if !b.reset {
		t.Fatal("reset flag should stay set until drained")
	}
	if len(b.data) != paneOutKeep+len(tail) {
		t.Fatalf("post-trim buffer is %d bytes, want tail+new (%d)", len(b.data), paneOutKeep+len(tail))
	}
	if !bytes.HasSuffix(b.data, tail) {
		t.Fatal("retained buffer must end in the most recent output")
	}
}
