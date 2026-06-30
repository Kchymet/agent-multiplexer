package daemon

import (
	"encoding/json"
	"net"
	"sync"

	"amux/internal/engine"
)

// connState is the daemon's per-connection state: a buffered write pump plus the
// set of engine panes this connection is currently streaming. Detaching a pane
// (explicitly or by disconnecting) only unsubscribes — the engine keeps the
// agent running, which is what lets a UI close and reopen without stopping work.
type connState struct {
	out  chan any
	done chan struct{}
	once sync.Once

	mu    sync.Mutex
	panes map[string]paneRoute // client pane id -> engine route
}

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
	}
	go cl.writeLoop(conn)
	return cl
}

// writeLoop is the single writer for this connection: it serializes every frame
// (snapshots, results, pane output) to the socket, so engine pump goroutines and
// the poll broadcaster never touch the connection directly.
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
		}
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
