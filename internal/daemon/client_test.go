package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
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

func TestNextRejectsOversizedFrameAndRetiresConnection(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer srv.Close()
	defer c.Close()

	go func() {
		_, _ = srv.Write(bytes.Repeat([]byte("x"), clientFrameLimit+1))
	}()
	if _, err := c.Next(); !errors.Is(err, ErrClientFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}
	select {
	case <-c.done:
	default:
		t.Fatal("oversized frame left client connection reusable")
	}
}

func TestNextContextCancellationInterruptsRead(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer srv.Close()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := c.NextContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled read error = %v", err)
	}
	select {
	case <-c.done:
	default:
		t.Fatal("interrupted partial-frame read left client connection reusable")
	}
}

func TestNextPreservesPaneReset(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer srv.Close()
	defer c.Close()

	go func() {
		_ = json.NewEncoder(srv).Encode(core.PaneFrame{Type: core.FramePaneReset, PaneID: "pane"})
	}()
	frame, err := c.Next()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Pane == nil || frame.Pane.Type != core.FramePaneReset || frame.Pane.PaneID != "pane" {
		t.Fatalf("pane reset decoded as %+v", frame)
	}
}

func TestPaneInputContextInterruptsBlockedWrite(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer srv.Close()
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := c.PaneInputContext(ctx, "pane", []byte("blocked")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled write error = %v", err)
	}
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("interrupted write did not retire client connection")
	}
}

func TestCloseInterruptsBlockedNext(t *testing.T) {
	srv, cli := net.Pipe()
	c := newClient(cli)
	defer srv.Close()

	result := make(chan error, 1)
	go func() {
		_, err := c.Next()
		result <- err
	}()
	time.Sleep(10 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blocked Next returned no error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt blocked Next")
	}
}

type observedWriteConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *observedWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

func TestCloseJoinsBlockedWriter(t *testing.T) {
	srv, raw := net.Pipe()
	cli := &observedWriteConn{Conn: raw, entered: make(chan struct{})}
	c := newClient(cli)
	defer srv.Close()

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- c.PaneInputContext(context.Background(), "pane", []byte("blocked"))
	}()
	<-cli.entered
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.writerDone:
	default:
		t.Fatal("Close returned before the blocked writer exited")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked write returned no error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write did not return after Close")
	}
}
