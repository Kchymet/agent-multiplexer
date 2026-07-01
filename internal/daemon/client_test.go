package daemon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"amux/internal/core"
)

// A wedged socket (the daemon not reading) must never block the caller of Send/
// PaneInput. The writer goroutine stalls on the first conn.Write, but the
// buffered queue absorbs the rest and drops once full — the UI goroutine and the
// vterm response-drain goroutine keep running. Before the async writer this call
// path did a blocking conn.Write, which is exactly what froze the native TUI.
func TestSendNeverBlocksWhenSocketStalls(t *testing.T) {
	// net.Pipe is synchronous: a Write blocks until the other end reads. We never
	// read from srv, so the writer goroutine is stuck from the first frame on.
	srv, cli := net.Pipe()
	defer srv.Close()
	c := newClient(cli)
	defer c.Close()

	done := make(chan struct{})
	go func() {
		// Far more than the outbound buffer, to prove overflow drops rather than
		// blocks. Every call must return promptly.
		for i := 0; i < outBuf*4; i++ {
			_ = c.PaneInput("p1", []byte("keystroke"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("PaneInput blocked on a stalled socket — the writer goroutine did not decouple the caller")
	}
}

// Byte ordering is preserved: frames enqueued in order reach the socket in the
// same order, because a single FIFO channel feeds a single writer goroutine.
func TestSendPreservesOrder(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer c.Close()
	defer srv.Close()

	const n = 50
	go func() {
		for i := 0; i < n; i++ {
			_ = c.PaneInput("p1", []byte{byte(i)})
		}
	}()

	_ = srv.SetReadDeadline(time.Now().Add(3 * time.Second))
	dec := json.NewDecoder(srv)
	for i := 0; i < n; i++ {
		var a core.Action
		if err := dec.Decode(&a); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if a.Action != core.ActionPaneInput || len(a.Data) != 1 || a.Data[0] != byte(i) {
			t.Fatalf("frame %d out of order: got action=%q data=%v", i, a.Action, a.Data)
		}
	}
}
