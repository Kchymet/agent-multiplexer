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
	src *source.Workspace

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

type client struct {
	conn  *muxproto.Conn
	out   chan muxproto.ServerMsg
	panes map[string]string // client pane id -> harness pane id
	sub   bool
}

// New creates a server.
func New() *Server {
	return &Server{
		src:     source.NewWorkspace(),
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
			r.cl.send(muxproto.ServerMsg{Type: muxproto.SPaneOutput, PaneID: r.clientPane, Data: m.Data})
		case harnessproto.HExit:
			r.cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: r.clientPane, Error: m.Error})
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
	cl := &client{conn: muxproto.NewConn(nc), out: make(chan muxproto.ServerMsg, 256), panes: map[string]string{}}
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
		s.handleMsg(cl, m)
	}
}

func (s *Server) handleMsg(cl *client, m muxproto.ClientMsg) {
	switch m.Type {
	case muxproto.CHello:
		host, _ := os.Hostname()
		cl.send(muxproto.ServerMsg{Type: muxproto.SWelcome, Version: muxproto.Version, Server: host})
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
	close(cl.out)
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

// send enqueues a message; a per-client writer goroutine drains it so a slow or
// remote client can't stall the harness or other clients.
func (cl *client) send(m muxproto.ServerMsg) {
	defer func() { _ = recover() }() // out may be closed during teardown
	select {
	case cl.out <- m:
	default:
		// Buffer full (very slow client): block briefly, then drop to avoid a
		// permanent stall. Terminal panes resync on the next full repaint.
		select {
		case cl.out <- m:
		case <-time.After(2 * time.Second):
		}
	}
}

func (cl *client) writeLoop() {
	for m := range cl.out {
		if err := cl.conn.WriteServer(m); err != nil {
			_ = cl.conn.Close()
			return
		}
	}
}

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
