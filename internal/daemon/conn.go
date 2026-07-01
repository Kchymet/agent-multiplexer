package daemon

import (
	"encoding/json"
	"net"
	"sync"

	"amux/internal/core"
	"amux/internal/engine"
)

// connState is the daemon's per-connection state: a buffered write pump plus the
// set of engine panes this connection is currently streaming. Detaching a pane
// (explicitly or by disconnecting) only unsubscribes — the engine keeps the
// agent running, which is what lets a UI close and reopen without stopping work.
//
// Two outbound paths share the one socket writer (writeLoop): discrete frames
// (snapshots, results) go through out and are droppable — each is a full state,
// so a slow client just misses an intermediate one. Pane output goes through the
// per-pane obuf and is LOSSLESS: a terminal byte stream is stateful, so dropping
// bytes from the middle (e.g. an erase sequence) corrupts the client's emulator
// and leaves ghost text. obuf coalesces bytes instead of dropping them; only a
// client that falls catastrophically far behind triggers a resync (see obuf).
type connState struct {
	out  chan any
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	panes map[string]paneRoute // client pane id -> engine route

	obMu sync.Mutex
	obuf map[string]*paneOut // client pane id -> pending lossless output
	wake chan struct{}       // nudges writeLoop that pane output is pending
}

// paneOut is one pane's pending output for a connection: coalesced bytes plus
// terminal flags. reset means the client must clear its emulator before applying
// data (a resync repaint); exit means the pane's process ended.
type paneOut struct {
	data    []byte
	reset   bool
	exit    bool
	exitErr string
}

const (
	// paneOutCap is how many unsent output bytes we coalesce for one pane before
	// giving up on streaming losslessly. It's generous (a wedged socket, not a
	// merely slow one, is what fills it); the engine's own scrollback is 256 KiB,
	// so this holds many full repaints.
	paneOutCap = 4 << 20
	// paneOutKeep is the recent tail retained on a resync. A full-screen agent
	// repaints within a frame, so the most recent repaint lives in this window;
	// after a reset the client rebuilds from it (a brief partial beats a ghost).
	paneOutKeep = 256 << 10
)

// paneRoute ties a client's pane id to the engine instance it streams and the
// unsubscribe handle for that stream.
type paneRoute struct {
	inst   engine.Instance
	cancel func()
}

func newConnState(conn net.Conn) *connState {
	cl := &connState{
		out:   make(chan any, 512),
		done:  make(chan struct{}),
		panes: map[string]paneRoute{},
		obuf:  map[string]*paneOut{},
		wake:  make(chan struct{}, 1),
	}
	go cl.writeLoop(conn)
	return cl
}

// writeLoop is the single writer for this connection: it serializes every frame
// (snapshots, results, pane output) to the socket, so engine pump goroutines and
// the poll broadcaster never touch the connection directly. Discrete frames
// arrive on out; pane output is drained losslessly from obuf when wake fires.
func (cl *connState) writeLoop(conn net.Conn) {
	enc := json.NewEncoder(conn)
	for {
		select {
		case <-cl.done:
			return
		case v := <-cl.out:
			if err := enc.Encode(v); err != nil {
				cl.stop()
				return
			}
		case <-cl.wake:
			if err := cl.drainPanes(enc); err != nil {
				cl.stop()
				return
			}
		}
	}
}

// drainPanes flushes every pane's pending output to the socket in order (reset,
// then bytes, then exit). enc.Encode may block on a slow socket; that only stalls
// this writer, never the engine pump — the pump appends into obuf under obMu and
// returns immediately. It runs until no pane has pending work.
func (cl *connState) drainPanes(enc *json.Encoder) error {
	for {
		cl.obMu.Lock()
		var paneID string
		var b *paneOut
		for id, p := range cl.obuf {
			if len(p.data) > 0 || p.reset || p.exit {
				paneID, b = id, p
				break
			}
		}
		if b == nil {
			cl.obMu.Unlock()
			return nil
		}
		reset, data, exit, exitErr := b.reset, b.data, b.exit, b.exitErr
		b.reset, b.data = false, nil
		if exit {
			delete(cl.obuf, paneID) // terminal: nothing more will arrive
		}
		cl.obMu.Unlock()

		if reset {
			if err := enc.Encode(core.PaneFrame{Type: core.FramePaneReset, PaneID: paneID}); err != nil {
				return err
			}
		}
		if len(data) > 0 {
			if err := enc.Encode(core.PaneFrame{Type: core.FramePaneOutput, PaneID: paneID, Data: data}); err != nil {
				return err
			}
		}
		if exit {
			if err := enc.Encode(core.PaneFrame{Type: core.FramePaneExit, PaneID: paneID, Error: exitErr}); err != nil {
				return err
			}
		}
	}
}

// paneOutput coalesces streamed output for a pane without blocking the engine
// pump that calls it. If the unsent backlog exceeds paneOutCap the client is
// hopelessly behind; rather than drop bytes from the middle of the stream (which
// corrupts the emulator), we keep only the recent tail and flag a reset so the
// client clears its screen before applying it.
func (cl *connState) paneOutput(paneID string, data []byte) {
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	b.data = append(b.data, data...)
	if len(b.data) > paneOutCap {
		tail := b.data[len(b.data)-paneOutKeep:]
		kept := make([]byte, len(tail)) // copy so the multi-MiB backing array is freed
		copy(kept, tail)
		b.data = kept
		b.reset = true
	}
	cl.obMu.Unlock()
	cl.signalWrite()
}

// paneExit records a pane's exit after any buffered output, so the client sees
// the final bytes before the exit frame. The route is forgotten immediately.
func (cl *connState) paneExit(paneID, exitErr string) {
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	b.exit, b.exitErr = true, exitErr
	cl.obMu.Unlock()
	cl.signalWrite()
	cl.dropRoute(paneID)
}

// signalWrite wakes writeLoop to drain pane output (coalesced, so one nudge
// covers any number of pending appends).
func (cl *connState) signalWrite() {
	select {
	case cl.wake <- struct{}{}:
	default:
	}
}

func (cl *connState) stop() { cl.once.Do(func() { close(cl.done) }) }

// send enqueues a frame without blocking. The channel is never closed, so a
// send racing with teardown can't panic; if the buffer is full (a stuck or slow
// client) the frame is dropped rather than stalling the engine — the terminal
// recovers on the next redraw, and a reattach replays the scrollback.
func (cl *connState) send(v any) {
	select {
	case cl.out <- v:
	case <-cl.done:
	default:
	}
}

// shutdown detaches every pane (without killing the agents) and stops the writer.
func (cl *connState) shutdown() {
	cl.mu.Lock()
	routes := cl.panes
	cl.panes = map[string]paneRoute{}
	cl.mu.Unlock()
	for _, r := range routes {
		r.cancel()
	}
	cl.stop()
}

func (cl *connState) addRoute(paneID string, r paneRoute) {
	cl.mu.Lock()
	cl.panes[paneID] = r
	cl.mu.Unlock()
}

func (cl *connState) route(paneID string) (paneRoute, bool) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	r, ok := cl.panes[paneID]
	return r, ok
}

func (cl *connState) paneInput(paneID string, data []byte) {
	if r, ok := cl.route(paneID); ok {
		r.inst.Input(data)
	}
}

func (cl *connState) paneResize(paneID string, cols, rows int) {
	if r, ok := cl.route(paneID); ok {
		r.inst.Resize(cols, rows)
	}
}

// paneClose detaches a pane: unsubscribe and forget it. The engine instance is
// left running.
func (cl *connState) paneClose(paneID string) {
	cl.mu.Lock()
	r, ok := cl.panes[paneID]
	delete(cl.panes, paneID)
	cl.mu.Unlock()
	cl.obMu.Lock()
	delete(cl.obuf, paneID) // drop any pending output for a detached pane
	cl.obMu.Unlock()
	if ok {
		r.cancel()
	}
}

// dropRoute forgets a pane whose instance has already exited (no cancel needed).
func (cl *connState) dropRoute(paneID string) {
	cl.mu.Lock()
	delete(cl.panes, paneID)
	cl.mu.Unlock()
}
