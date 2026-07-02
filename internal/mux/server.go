// Package mux is amux's multiplexer server: the backend half of the client/server
// split. It speaks muxproto to any number of UI clients (local unix socket or
// remote TCP), owns the session model via store/source/wsops, and routes agent
// pane I/O between clients and an agent harness (harnessproto). See
// docs/client-server.md.
package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/harness"
	"amux/internal/harnessproto"
	"amux/internal/muxproto"
	"amux/internal/panespec"
	"amux/internal/source"
	"amux/internal/wsops"
)

// Server is a running multiplexer server.
type Server struct {
	src   *source.Workspace
	token string // bearer token required of clients; empty disables auth

	mu       sync.Mutex
	clients  map[*client]bool
	routes   map[string]route // harness pane id -> owning client + its pane id
	hconn    *harnessproto.Conn
	paneSeq  int64
	lastSnap []byte // last broadcast snapshot, for change detection

	pollCh chan struct{}
}

type route struct {
	cl         *client
	clientPane string
}

// client is one connected UI. Two outbound paths share the single socket writer
// (writeLoop), mirroring internal/daemon/conn.go: discrete frames (welcome,
// snapshot, result) go through out and are DROPPABLE — each is a full state, so a
// slow client just misses an intermediate one. Pane output goes through the
// per-pane obuf and is LOSSLESS: a terminal byte stream is stateful, so dropping
// bytes from the middle (e.g. an erase sequence) corrupts the client's emulator
// and ghosts text. obuf coalesces instead of dropping; only a client that falls
// catastrophically far behind (past paneOutCap) triggers a trim-to-tail + reset.
type client struct {
	conn  *muxproto.Conn
	out   chan muxproto.ServerMsg
	done  chan struct{}
	once  sync.Once
	panes map[string]string // client pane id -> harness pane id
	sub   bool

	obMu sync.Mutex
	obuf map[string]*paneOut // client pane id -> pending lossless output
	wake chan struct{}       // nudges writeLoop that pane output is pending
}

// paneOut is one pane's pending output for a client: coalesced bytes plus
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
	// giving up on streaming losslessly (a wedged socket, not a merely slow one,
	// is what fills it). Matches the replay cap in docs/remote-provider.md.
	paneOutCap = 4 << 20
	// paneOutKeep is the recent tail retained on a resync; a full-screen agent
	// repaints within it, so after a reset the client rebuilds from the tail.
	paneOutKeep = 256 << 10
)

// New creates a server. When $AMUX_MUX_TOKEN is set, clients must present a
// matching token in their hello (constant-time checked); an empty value leaves
// auth off, appropriate for the trusted local unix socket.
func New() *Server {
	return &Server{
		src:     source.NewWorkspace(),
		token:   os.Getenv("AMUX_MUX_TOKEN"),
		clients: map[*client]bool{},
		routes:  map[string]route{},
		pollCh:  make(chan struct{}, 1),
	}
}

// Serve starts the harness and the poll loop, then accepts clients on every
// listener until ctx is cancelled. Blocks.
func (s *Server) Serve(ctx context.Context, lns ...net.Listener) error {
	s.startHarness()
	go s.pollLoop(ctx)
	for _, ln := range lns {
		go s.acceptLoop(ln)
	}
	<-ctx.Done()
	return nil
}

// ---- harness (in-process over net.Pipe; the protocol is real either way) ----

func (s *Server) startHarness() {
	a, b := net.Pipe()
	s.hconn = harnessproto.NewConn(a)
	go func() { _ = harness.Serve(harnessproto.NewConn(b)) }()
	if r, err := s.hconn.ReadHarness(); err != nil || r.Type != harnessproto.HReady {
		return
	}
	go s.readHarness()
}

// readHarness routes harness output/exit frames to the client that owns the pane.
func (s *Server) readHarness() {
	for {
		m, err := s.hconn.ReadHarness()
		if err != nil {
			return
		}
		r, ok := s.lookup(m.PaneID)
		if !ok {
			continue
		}
		switch m.Type {
		case harnessproto.HOutput:
			// Lossless: coalesce into the per-pane buffer, never drop bytes.
			r.cl.paneOutput(r.clientPane, m.Data)
		case harnessproto.HExit:
			// Ordered after any buffered output so the client sees final bytes first.
			r.cl.paneExit(r.clientPane, m.Error)
			s.mu.Lock()
			delete(s.routes, m.PaneID)
			delete(r.cl.panes, r.clientPane)
			s.mu.Unlock()
		}
	}
}

func (s *Server) lookup(harnessPane string) (route, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.routes[harnessPane]
	return r, ok
}

// ---- clients ----

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go s.handleClient(c)
	}
}

func (s *Server) handleClient(nc net.Conn) {
	cl := &client{
		conn:  muxproto.NewConn(nc),
		out:   make(chan muxproto.ServerMsg, 256),
		done:  make(chan struct{}),
		panes: map[string]string{},
		obuf:  map[string]*paneOut{},
		wake:  make(chan struct{}, 1),
	}
	s.mu.Lock()
	s.clients[cl] = true
	s.mu.Unlock()
	go cl.writeLoop()
	defer s.dropClient(cl)
	for {
		m, err := cl.conn.ReadClient()
		if err != nil {
			return
		}
		if !s.handleMsg(cl, m) {
			return // terminal (auth/version reject): stop reading, tear down
		}
	}
}

// handleMsg processes one client message; it returns false when the connection
// must be torn down (a terminal hello rejection).
func (s *Server) handleMsg(cl *client, m muxproto.ClientMsg) bool {
	switch m.Type {
	case muxproto.CHello:
		// Version negotiation: a single supported version, so any mismatch has no
		// overlap and fails loudly. Auth is a constant-time token compare.
		if m.Version != muxproto.Version {
			cl.reject(muxproto.ErrBadVersion)
			return false
		}
		if !muxproto.TokenOK(s.token, m.Token) {
			cl.reject(muxproto.ErrBadToken)
			return false
		}
		host, _ := os.Hostname()
		cl.send(muxproto.ServerMsg{Type: muxproto.SWelcome, OK: true, Version: muxproto.Version, Server: host})
	case muxproto.CSubscribe:
		s.mu.Lock()
		cl.sub = true
		s.mu.Unlock()
		if sess, err := s.src.Poll(context.Background()); err == nil {
			cl.send(muxproto.ServerMsg{Type: muxproto.SSnapshot, Sessions: sess})
		}
	case muxproto.CAction:
		err := wsops.Apply(context.Background(), core.Action{Action: m.Action, ID: m.ID, Target: m.Target, Fields: m.Fields})
		res := muxproto.ServerMsg{Type: muxproto.SResult, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
		}
		cl.send(res)
		s.pollNow()
	case muxproto.CPaneOpen:
		s.openPane(cl, m)
	case muxproto.CPaneInput:
		if hp := s.harnessPane(cl, m.PaneID); hp != "" {
			_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MInput, PaneID: hp, Data: m.Data})
		}
	case muxproto.CPaneResize:
		if hp := s.harnessPane(cl, m.PaneID); hp != "" {
			_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MResize, PaneID: hp, Cols: m.Cols, Rows: m.Rows})
		}
	case muxproto.CPaneClose:
		s.closePane(cl, m.PaneID)
	}
	return true
}

func (s *Server) openPane(cl *client, m muxproto.ClientMsg) {
	dir, env, argv, err := panespec.Resolve(m.Agent, m.Tab)
	if err != nil {
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: err.Error()})
		return
	}
	env = append(env, "TERM=xterm-256color")
	s.mu.Lock()
	s.paneSeq++
	hp := "h" + itoa(s.paneSeq)
	s.routes[hp] = route{cl: cl, clientPane: m.PaneID}
	cl.panes[m.PaneID] = hp
	s.mu.Unlock()
	_ = s.hconn.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: hp, Dir: dir, Env: env, Argv: argv, Cols: m.Cols, Rows: m.Rows,
	})
}

func (s *Server) closePane(cl *client, clientPane string) {
	s.mu.Lock()
	hp := cl.panes[clientPane]
	delete(cl.panes, clientPane)
	delete(s.routes, hp)
	s.mu.Unlock()
	cl.obMu.Lock()
	delete(cl.obuf, clientPane) // drop any pending output for a detached pane
	cl.obMu.Unlock()
	if hp != "" {
		_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MKill, PaneID: hp})
	}
}

func (s *Server) harnessPane(cl *client, clientPane string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cl.panes[clientPane]
}

func (s *Server) dropClient(cl *client) {
	s.mu.Lock()
	delete(s.clients, cl)
	var kill []string
	for _, hp := range cl.panes {
		kill = append(kill, hp)
		delete(s.routes, hp)
	}
	cl.panes = map[string]string{}
	s.mu.Unlock()
	for _, hp := range kill {
		_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MKill, PaneID: hp})
	}
	cl.stop()
}

// ---- snapshots ----

func (s *Server) pollLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.pollCh:
		}
		s.broadcast()
	}
}

func (s *Server) pollNow() {
	select {
	case s.pollCh <- struct{}{}:
	default:
	}
}

func (s *Server) broadcast() {
	sess, err := s.src.Poll(context.Background())
	if err != nil {
		return
	}
	b, _ := json.Marshal(sess)
	s.mu.Lock()
	changed := !bytes.Equal(b, s.lastSnap)
	s.lastSnap = b
	var subs []*client
	for cl := range s.clients {
		if cl.sub {
			subs = append(subs, cl)
		}
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	msg := muxproto.ServerMsg{Type: muxproto.SSnapshot, Sessions: sess}
	for _, cl := range subs {
		cl.send(msg)
	}
}

// ---- client write pump ----

// writeLoop is the single writer for this client: it serializes every frame to
// the socket, so the harness reader and the poll broadcaster never touch the
// connection directly. Discrete frames arrive on out; lossless pane output is
// drained from obuf when wake fires. On a write error or teardown it closes the
// connection, which unblocks the reader.
func (cl *client) writeLoop() {
	for {
		select {
		case <-cl.done:
			_ = cl.conn.Close()
			return
		case m := <-cl.out:
			if err := cl.conn.WriteServer(m); err != nil {
				cl.stop()
				_ = cl.conn.Close()
				return
			}
		case <-cl.wake:
			if err := cl.drainPanes(); err != nil {
				cl.stop()
				_ = cl.conn.Close()
				return
			}
		}
	}
}

// send enqueues a discrete frame without blocking. The channel is never closed,
// so a send racing with teardown can't panic; if the buffer is full (a stuck or
// slow client) the frame is dropped rather than stalling the harness — each such
// frame is a full state the client recovers on the next one.
func (cl *client) send(m muxproto.ServerMsg) {
	select {
	case cl.out <- m:
	case <-cl.done:
	default:
	}
}

// reject writes a terminal welcome synchronously (so it reaches the client
// before the socket closes) and tears the connection down.
func (cl *client) reject(errCode string) {
	_ = cl.conn.WriteServer(muxproto.ServerMsg{Type: muxproto.SWelcome, OK: false, Error: errCode})
	cl.stop()
}

// paneOutput coalesces streamed output for a pane without blocking the harness
// reader that calls it. If the unsent backlog exceeds paneOutCap the client is
// hopelessly behind; rather than drop bytes from the middle of the stream (which
// corrupts the emulator), we keep only the recent tail and flag a reset so the
// client clears its screen before applying it.
func (cl *client) paneOutput(paneID string, data []byte) {
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
// the final bytes before the exit frame.
func (cl *client) paneExit(paneID, exitErr string) {
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	b.exit, b.exitErr = true, exitErr
	cl.obMu.Unlock()
	cl.signalWrite()
}

// drainPanes flushes every pane's pending output to the socket in order (reset,
// then bytes, then exit). WriteServer may block on a slow socket; that only
// stalls this writer, never the harness reader — which appends into obuf under
// obMu and returns immediately. Runs until no pane has pending work.
func (cl *client) drainPanes() error {
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
			if err := cl.conn.WriteServer(muxproto.ServerMsg{Type: muxproto.SPaneReset, PaneID: paneID}); err != nil {
				return err
			}
		}
		if len(data) > 0 {
			if err := cl.conn.WriteServer(muxproto.ServerMsg{Type: muxproto.SPaneOutput, PaneID: paneID, Data: data}); err != nil {
				return err
			}
		}
		if exit {
			if err := cl.conn.WriteServer(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: paneID, Error: exitErr}); err != nil {
				return err
			}
		}
	}
}

// signalWrite wakes writeLoop to drain pane output (coalesced, so one nudge
// covers any number of pending appends).
func (cl *client) signalWrite() {
	select {
	case cl.wake <- struct{}{}:
	default:
	}
}

// stop signals writeLoop to exit; idempotent.
func (cl *client) stop() { cl.once.Do(func() { close(cl.done) }) }

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
