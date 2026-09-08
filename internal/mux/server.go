// Package mux is amux's multiplexer server: the backend half of the client/server
// split. It speaks muxproto to authenticated UI clients, relays authoritative
// state/control to the primary daemon, and bridges primary-owned pane streams to
// legacy clients. See docs/client-server.md.
package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/muxproto"
	"amux/internal/panespec"
)

// Server is a running multiplexer server.
type Server struct {
	primary Primary
	token   string // nonempty bearer required inside an authenticated TLS channel

	mu           sync.Mutex
	clients      map[*client]bool
	routes       map[*route]bool
	epoch        uint64
	lastSnap     []byte // last broadcast snapshot, for change detection
	lastSessions []core.Session

	pollCh chan struct{}
}

// PaneRequest is the only pane authority the mux may pass upstream. It contains
// the daemon wire's already-public target and viewport fields, never a launch
// path, environment, argv, credential, or access grant.
type PaneRequest struct {
	Agent      string
	Tab        int
	Cols, Rows int
}

// PaneRelay is one primary-daemon-owned pane stream. Closing it detaches this
// subscriber and interrupts a blocked Next/Input/Resize operation; it does not
// kill the daemon-owned agent runtime.
type PaneRelay interface {
	Next(context.Context) (core.PaneFrame, error)
	Input([]byte) error
	Resize(cols, rows int) error
	Close() error
}

// Primary is the mux's only state/authority seam. Production implements it by
// authenticating to the singleton daemon as the host for every operation. Tests
// inject inert fakes; no mux path opens the store or a FileAuthority.
type Primary interface {
	Snapshot(context.Context) ([]core.Session, error)
	Dispatch(context.Context, core.Action) (string, error)
	OpenPane(context.Context, PaneRequest) (PaneRelay, error)
}

type unavailablePrimary struct{}

func (unavailablePrimary) Snapshot(context.Context) ([]core.Session, error) {
	return nil, fmt.Errorf("legacy mux has no authenticated primary daemon relay")
}
func (unavailablePrimary) Dispatch(context.Context, core.Action) (string, error) {
	return "", fmt.Errorf("legacy mux has no authenticated primary daemon relay")
}
func (unavailablePrimary) OpenPane(context.Context, PaneRequest) (PaneRelay, error) {
	return nil, fmt.Errorf("legacy mux has no authenticated primary pane relay")
}

type route struct {
	cl         *client
	clientPane string
	agent      string
	epoch      uint64
	ctx        context.Context
	cancel     context.CancelFunc
	relay      PaneRelay
	done       chan struct{}
	doneOnce   sync.Once
}

func (r *route) finish() { r.doneOnce.Do(func() { close(r.done) }) }

// client is one connected UI. Two outbound paths share the single socket writer
// (writeLoop), mirroring internal/daemon/conn.go: discrete frames (welcome,
// snapshot, result) go through out and are DROPPABLE — each is a full state, so a
// slow client just misses an intermediate one. Pane output goes through the
// per-pane obuf and is LOSSLESS: a terminal byte stream is stateful, so dropping
// bytes from the middle (e.g. an erase sequence) corrupts the client's emulator
// and ghosts text. obuf coalesces instead of dropping; only a client that falls
// catastrophically far behind (past paneOutCap) triggers a trim-to-tail + reset.
type client struct {
	conn       *muxproto.Conn
	raw        net.Conn
	out        chan muxproto.ServerMsg
	done       chan struct{}
	writerDone chan struct{}
	once       sync.Once
	server     *Server
	epoch      uint64
	panes      map[string]*route // client pane id -> primary pane relay
	sub        bool
	writeMu    sync.Mutex // target revocation waits for an already-started frame

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
	route   *route // nil only in transport-only tests
}

const (
	primaryReadTimeout     = 5 * time.Second
	primaryActionTimeout   = 30 * time.Second
	downstreamWriteTimeout = 5 * time.Second
	// paneOutCap is how many unsent output bytes we coalesce for one pane before
	// giving up on streaming losslessly (a wedged socket, not a merely slow one,
	// is what fills it). Matches the replay cap in docs/remote-provider.md.
	paneOutCap = 4 << 20
	// paneOutKeep is the recent tail retained on a resync; a full-screen agent
	// repaints within it, so after a reset the client rebuilds from the tail.
	paneOutKeep = 256 << 10
)

// New creates a server backed only by an injected primary-daemon relay. A
// missing relay is fail-closed. Client authentication is mandatory: an empty
// AMUX_MUX_TOKEN never enables a trusted-local bypass.
func New(primaries ...Primary) *Server {
	var primary Primary = unavailablePrimary{}
	if len(primaries) != 0 && primaries[0] != nil {
		primary = primaries[0]
	}
	return &Server{
		primary: primary,
		token:   os.Getenv("AMUX_MUX_TOKEN"),
		clients: map[*client]bool{},
		routes:  map[*route]bool{},
		epoch:   1,
		pollCh:  make(chan struct{}, 1),
	}
}

// Serve starts the poll loop and accepts clients until ctx is cancelled. The
// primary daemon owns every process and PTY; there is no embedded harness to
// outlive this relay.
func (s *Server) Serve(ctx context.Context, lns ...net.Listener) error {
	defer s.suspend()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		s.pollLoop(ctx)
	}()
	for _, ln := range lns {
		go s.acceptLoop(ln)
	}
	<-ctx.Done()
	<-pollDone
	return nil
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
		conn:       muxproto.NewConn(nc),
		raw:        nc,
		out:        make(chan muxproto.ServerMsg, 256),
		done:       make(chan struct{}),
		server:     s,
		panes:      map[string]*route{},
		obuf:       map[string]*paneOut{},
		wake:       make(chan struct{}, 1),
		writerDone: make(chan struct{}),
	}
	go cl.writeLoop()
	// Authentication is a strict first-frame gate. Before it succeeds the client
	// is absent from subscriptions and no asynchronous writer exists that could
	// disclose a snapshot or pane byte.
	_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	hello, err := cl.conn.ReadClient()
	if err != nil || hello.Type != muxproto.CHello || hello.Version != muxproto.Version ||
		strings.TrimSpace(s.token) == "" || !muxproto.TokenOK(s.token, hello.Token) {
		cl.reject(muxproto.ErrUnauthorized)
		_ = cl.conn.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	// The mux bearer is not primary-daemon authority. Admit the connection only
	// after a fresh authenticated primary read; this also prevents new clients
	// from entering while a prior poll failure has suspended cached grants.
	s.mu.Lock()
	admissionEpoch := s.epoch
	s.mu.Unlock()
	authCtx, cancelAuth := context.WithTimeout(context.Background(), primaryReadTimeout)
	sessions, err := s.primary.Snapshot(authCtx)
	cancelAuth()
	if err != nil {
		cl.reject(muxproto.ErrUnauthorized)
		_ = cl.conn.Close()
		return
	}
	s.mu.Lock()
	if s.epoch != admissionEpoch {
		s.mu.Unlock()
		cl.reject(muxproto.ErrUnauthorized)
		_ = cl.conn.Close()
		return
	}
	cl.epoch = admissionEpoch
	s.clients[cl] = true
	revoked := s.rememberLocked(sessions)
	s.mu.Unlock()
	closeRoutes(revoked, true)
	if !s.clientActive(cl) {
		return
	}
	host, _ := os.Hostname()
	if err := cl.writeServer(muxproto.ServerMsg{Type: muxproto.SWelcome, OK: true, Version: muxproto.Version, Server: host}); err != nil {
		s.dropClient(cl)
		return
	}
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
	if !s.clientActive(cl) {
		return false
	}
	switch m.Type {
	case muxproto.CHello:
		return false // hello is valid exactly once and only as the first frame
	case muxproto.CSubscribe:
		ctx, cancel := context.WithTimeout(context.Background(), primaryReadTimeout)
		sess, err := s.primary.Snapshot(ctx)
		cancel()
		if err == nil {
			revoked, active := s.rememberFor(cl, sess)
			closeRoutes(revoked, true)
			if !active || !s.clientActive(cl) {
				return false
			}
			s.mu.Lock()
			cl.sub = true
			s.mu.Unlock()
			cl.send(muxproto.ServerMsg{Type: muxproto.SSnapshot, Sessions: sess})
		} else {
			s.suspend()
			return false
		}
	case muxproto.CAction:
		act := core.Action{Action: m.Action, ID: m.ID, Target: m.Target, Fields: m.Fields}
		// The compatibility token grants only the public control vocabulary. In
		// particular, it must not turn daemon-internal host operations (shutdown,
		// runtime recreation, queries, or pane frames) into remote actions merely
		// because this relay itself authenticates as a host upstream.
		if !core.KnownAction(act.Action) {
			cl.send(muxproto.ServerMsg{Type: muxproto.SResult, OK: false, Error: "unsupported action"})
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), primaryActionTimeout)
		newID, err := s.primary.Dispatch(ctx, act)
		cancel()
		if !s.clientActive(cl) {
			return false
		}
		res := muxproto.ServerMsg{Type: muxproto.SResult, OK: err == nil, NewID: newID}
		if err != nil {
			res.Error = err.Error()
		}
		cl.send(res)
		s.pollNow()
	case muxproto.CPaneOpen:
		s.openPane(cl, m)
	case muxproto.CPaneInput:
		if r := s.clientRoute(cl, m.PaneID); r != nil {
			if err := r.relay.Input(m.Data); err != nil {
				s.closeRoute(r, true)
			}
		}
	case muxproto.CPaneResize:
		if r := s.clientRoute(cl, m.PaneID); r != nil {
			if err := r.relay.Resize(m.Cols, m.Rows); err != nil {
				s.closeRoute(r, true)
			}
		}
	case muxproto.CPaneClose:
		s.closePane(cl, m.PaneID)
	default:
		return false
	}
	return true
}

func (s *Server) openPane(cl *client, m muxproto.ClientMsg) {
	if strings.TrimSpace(m.PaneID) == "" || strings.TrimSpace(m.Agent) == "" ||
		m.Tab < panespec.TabAgent || m.Tab > panespec.TabTerminal {
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: "invalid pane request"})
		return
	}
	s.mu.Lock()
	if !s.clientActiveLocked(cl) {
		s.mu.Unlock()
		return
	}
	if !s.sessionActiveLocked(m.Agent) {
		s.mu.Unlock()
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: "pane target is not active"})
		return
	}
	if _, duplicate := cl.panes[m.PaneID]; duplicate {
		s.mu.Unlock()
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: "pane id already open"})
		return
	}
	routeCtx, routeCancel := context.WithCancel(context.Background())
	openCtx, cancelOpen := context.WithTimeout(routeCtx, primaryActionTimeout)
	r := &route{
		cl: cl, clientPane: m.PaneID, agent: m.Agent, epoch: cl.epoch,
		ctx: routeCtx, cancel: routeCancel, done: make(chan struct{}),
	}
	cl.panes[m.PaneID] = r // reserve the connection-scoped id across the blocking open
	s.routes[r] = true
	s.mu.Unlock()

	relay, err := s.primary.OpenPane(openCtx, PaneRequest{Agent: m.Agent, Tab: m.Tab, Cols: m.Cols, Rows: m.Rows})
	cancelOpen()
	if err != nil {
		s.failOpeningRoute(r, err)
		return
	}
	s.mu.Lock()
	if !s.clientActiveLocked(cl) || cl.panes[m.PaneID] != r || r.epoch != s.epoch {
		s.mu.Unlock()
		_ = relay.Close()
		r.cancel()
		r.finish()
		return
	}
	r.relay = relay
	s.mu.Unlock()
	go s.pumpRoute(r)
}

func (s *Server) closePane(cl *client, clientPane string) {
	s.mu.Lock()
	r := cl.panes[clientPane]
	active := s.clientActiveLocked(cl)
	s.mu.Unlock()
	if active && r != nil {
		s.closeRoute(r, true)
	}
}

func (s *Server) failOpeningRoute(r *route, err error) {
	s.mu.Lock()
	current := s.routes[r] && r.cl.panes[r.clientPane] == r && s.clientActiveLocked(r.cl)
	delete(s.routes, r)
	if r.cl.panes[r.clientPane] == r {
		delete(r.cl.panes, r.clientPane)
	}
	s.mu.Unlock()
	r.cancel()
	r.finish()
	if current {
		r.cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: r.clientPane, Error: err.Error()})
	}
}

func (s *Server) pumpRoute(r *route) {
	defer r.finish()
	defer r.cancel()
	defer func() { _ = r.relay.Close() }()
	for {
		frame, err := r.relay.Next(r.ctx)
		if err != nil {
			if r.ctx.Err() == nil && s.routeCurrent(r) {
				r.cl.paneExitRoute(r, err.Error())
				return
			}
			s.forgetRoute(r)
			return
		}
		if !s.routeCurrent(r) {
			return
		}
		switch frame.Type {
		case core.FramePaneReset:
			r.cl.paneResetRoute(r)
		case core.FramePaneOutput:
			r.cl.paneOutputRoute(r, frame.Data)
		case core.FramePaneExit:
			r.cl.paneExitRoute(r, frame.Error)
			return
		}
	}
}

func (s *Server) routeCurrent(r *route) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[r] && s.clientActiveLocked(r.cl) && r.cl.panes[r.clientPane] == r && r.epoch == s.epoch
}

func (s *Server) forgetRoute(r *route) {
	s.mu.Lock()
	delete(s.routes, r)
	if r.cl.panes[r.clientPane] == r {
		delete(r.cl.panes, r.clientPane)
	}
	s.mu.Unlock()
}

func (s *Server) closeRoute(r *route, wait bool) {
	s.forgetRoute(r)
	r.cancel()
	if r.relay != nil {
		_ = r.relay.Close()
	} else {
		// An opening route owns no pump yet. Its OpenPane call must honor ctx and
		// closes done as it unwinds through failOpeningRoute/stale admission.
	}
	r.cl.obMu.Lock()
	delete(r.cl.obuf, r.clientPane)
	r.cl.obMu.Unlock()
	r.cl.writeMu.Lock()
	r.cl.writeMu.Unlock()
	if wait {
		<-r.done
	}
}

func closeRoutes(routes []*route, wait bool) {
	clients := make(map[*client]bool)
	for _, r := range routes {
		r.cancel()
		if r.relay != nil {
			_ = r.relay.Close()
		}
		r.cl.obMu.Lock()
		delete(r.cl.obuf, r.clientPane)
		r.cl.obMu.Unlock()
		clients[r.cl] = true
	}
	for cl := range clients {
		cl.writeMu.Lock()
		cl.writeMu.Unlock()
	}
	if wait {
		for _, r := range routes {
			<-r.done
		}
	}
}

func (s *Server) clientRoute(cl *client, clientPane string) *route {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clientActiveLocked(cl) {
		return nil
	}
	r := cl.panes[clientPane]
	if r == nil || r.relay == nil || r.epoch != s.epoch {
		return nil
	}
	return r
}

func (s *Server) dropClient(cl *client) {
	s.mu.Lock()
	delete(s.clients, cl)
	var routes []*route
	for _, r := range cl.panes {
		delete(s.routes, r)
		routes = append(routes, r)
	}
	cl.panes = map[string]*route{}
	s.mu.Unlock()
	cl.stop()
	if cl.writerDone != nil {
		<-cl.writerDone
	}
	closeRoutes(routes, true)
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
		s.broadcast(ctx)
	}
}

func (s *Server) pollNow() {
	select {
	case s.pollCh <- struct{}{}:
	default:
	}
}

func (s *Server) broadcast(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, primaryReadTimeout)
	sess, err := s.primary.Snapshot(ctx)
	cancel()
	if err != nil {
		s.suspend()
		return
	}
	b, _ := json.Marshal(sess)
	s.mu.Lock()
	changed := !bytes.Equal(b, s.lastSnap)
	revoked := s.rememberLocked(sess)
	var subs []*client
	for cl := range s.clients {
		if cl.sub {
			subs = append(subs, cl)
		}
	}
	s.mu.Unlock()
	// Route revocation commits before the reduced snapshot is emitted. Closing
	// both directions interrupts blocked reads/writes; join prevents pane-id reuse
	// from racing a stale primary frame.
	closeRoutes(revoked, true)
	if !changed {
		return
	}
	msg := muxproto.ServerMsg{Type: muxproto.SSnapshot, Sessions: sess}
	for _, cl := range subs {
		cl.send(msg)
	}
}

func activeSessionIDs(sessions []core.Session) map[string]bool {
	active := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		if session.ID != "" && !session.Archived {
			active[session.ID] = true
		}
	}
	return active
}

// suspend revokes every cached grant when the primary daemon can no longer be
// authenticated/read. Existing clients are disconnected and every upstream
// pane relay is detached; the primary daemon remains the runtime owner. A later
// successful poll does not resurrect either client or route.
func (s *Server) suspend() {
	s.mu.Lock()
	s.epoch++
	clients := make([]*client, 0, len(s.clients))
	for cl := range s.clients {
		clients = append(clients, cl)
		cl.panes = map[string]*route{}
	}
	routes := make([]*route, 0, len(s.routes))
	for r := range s.routes {
		routes = append(routes, r)
	}
	s.clients = map[*client]bool{}
	s.routes = map[*route]bool{}
	s.lastSnap = nil
	s.lastSessions = nil
	s.mu.Unlock()
	for _, cl := range clients {
		cl.stop()
	}
	for _, cl := range clients {
		if cl.writerDone != nil {
			<-cl.writerDone
		}
	}
	closeRoutes(routes, true)
}

func (s *Server) remember(sessions []core.Session) {
	s.mu.Lock()
	revoked := s.rememberLocked(sessions)
	s.mu.Unlock()
	closeRoutes(revoked, true)
}

func (s *Server) rememberLocked(sessions []core.Session) []*route {
	b, _ := json.Marshal(sessions)
	s.lastSnap = b
	s.lastSessions = append(s.lastSessions[:0], sessions...)
	active := activeSessionIDs(sessions)
	var revoked []*route
	for r := range s.routes {
		if !active[r.agent] {
			delete(s.routes, r)
			if r.cl.panes[r.clientPane] == r {
				delete(r.cl.panes, r.clientPane)
			}
			revoked = append(revoked, r)
		}
	}
	return revoked
}

func (s *Server) rememberFor(cl *client, sessions []core.Session) ([]*route, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clientActiveLocked(cl) {
		return nil, false
	}
	return s.rememberLocked(sessions), true
}

func (s *Server) clientActive(cl *client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientActiveLocked(cl)
}

func (s *Server) clientActiveLocked(cl *client) bool {
	return s.clients[cl] && cl.epoch == s.epoch
}

func (s *Server) sessionActiveLocked(id string) bool {
	for _, session := range s.lastSessions {
		if session.ID == id && !session.Archived {
			return true
		}
	}
	return false
}

func (s *Server) sessions() []core.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Session(nil), s.lastSessions...)
}

// ---- client write pump ----

// writeLoop is the single writer for this client: it serializes every frame to
// the socket, so primary pane pumps and the poll broadcaster never touch the
// connection directly. Discrete frames arrive on out; lossless pane output is
// drained from obuf when wake fires. On a write error or teardown it closes the
// connection, which unblocks the reader.
func (cl *client) writeLoop() {
	if cl.writerDone != nil {
		defer close(cl.writerDone)
	}
	for {
		select {
		case <-cl.done:
			return
		case m := <-cl.out:
			if !cl.authorized() {
				cl.stop()
				return
			}
			if err := cl.writeServer(m); err != nil {
				cl.stop()
				return
			}
		case <-cl.wake:
			if !cl.authorized() {
				cl.stop()
				return
			}
			if err := cl.drainPanes(); err != nil {
				cl.stop()
				return
			}
		}
	}
}

// send enqueues a discrete frame without blocking. The channel is never closed,
// so a send racing with teardown can't panic; if the buffer is full (a stuck or
// slow client) the frame is dropped rather than stalling a primary pane pump — each such
// frame is a full state the client recovers on the next one.
func (cl *client) send(m muxproto.ServerMsg) {
	if !cl.authorized() {
		return
	}
	select {
	case cl.out <- m:
	case <-cl.done:
	default:
	}
}

// reject writes a terminal welcome synchronously (so it reaches the client
// before the socket closes) and tears the connection down.
func (cl *client) reject(errCode string) {
	_ = cl.writeServer(muxproto.ServerMsg{Type: muxproto.SWelcome, OK: false, Error: errCode})
	cl.stop()
}

// paneOutput coalesces streamed output without blocking its primary pane pump.
// If the unsent backlog exceeds paneOutCap the client is
// hopelessly behind; rather than drop bytes from the middle of the stream (which
// corrupts the emulator), we keep only the recent tail and flag a reset so the
// client clears its screen before applying it.
func (cl *client) paneOutput(paneID string, data []byte) {
	cl.paneOutputFor(paneID, nil, data)
}

func (cl *client) paneOutputRoute(r *route, data []byte) {
	cl.paneOutputFor(r.clientPane, r, data)
}

func (cl *client) paneOutputFor(paneID string, route *route, data []byte) {
	if !cl.authorized() {
		return
	}
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	if route != nil {
		b.route = route
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

func (cl *client) paneReset(paneID string) {
	cl.paneResetFor(paneID, nil)
}

func (cl *client) paneResetRoute(r *route) {
	cl.paneResetFor(r.clientPane, r)
}

func (cl *client) paneResetFor(paneID string, route *route) {
	if !cl.authorized() {
		return
	}
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	if route != nil {
		b.route = route
	}
	b.reset = true
	cl.obMu.Unlock()
	cl.signalWrite()
}

// paneExit records a pane's exit after any buffered output, so the client sees
// the final bytes before the exit frame.
func (cl *client) paneExit(paneID, exitErr string) {
	cl.paneExitFor(paneID, nil, exitErr)
}

func (cl *client) paneExitRoute(r *route, exitErr string) {
	cl.paneExitFor(r.clientPane, r, exitErr)
}

func (cl *client) paneExitFor(paneID string, route *route, exitErr string) {
	if !cl.authorized() {
		return
	}
	cl.obMu.Lock()
	b := cl.obuf[paneID]
	if b == nil {
		b = &paneOut{}
		cl.obuf[paneID] = b
	}
	if route != nil {
		b.route = route
	}
	b.exit, b.exitErr = true, exitErr
	cl.obMu.Unlock()
	cl.signalWrite()
}

// drainPanes flushes every pane's pending output to the socket in order (reset,
// then bytes, then exit). WriteServer may block on a slow socket; that only
// stalls this writer, never a primary pane pump — which appends into obuf under
// obMu and returns immediately. Runs until no pane has pending work.
func (cl *client) drainPanes() error {
	for {
		if !cl.authorized() {
			return net.ErrClosed
		}
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
		reset, data, exit, exitErr, route := b.reset, b.data, b.exit, b.exitErr, b.route
		b.reset, b.data = false, nil
		if exit {
			delete(cl.obuf, paneID) // terminal: nothing more will arrive
		}
		cl.obMu.Unlock()

		if reset {
			if _, err := cl.writePane(route, muxproto.ServerMsg{Type: muxproto.SPaneReset, PaneID: paneID}); err != nil {
				return err
			}
		}
		if len(data) > 0 {
			if _, err := cl.writePane(route, muxproto.ServerMsg{Type: muxproto.SPaneOutput, PaneID: paneID, Data: data}); err != nil {
				return err
			}
		}
		if exit {
			delivered, err := cl.writePane(route, muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: paneID, Error: exitErr})
			if err != nil {
				return err
			}
			// A natural primary-pane exit keeps the client pane id reserved until
			// the terminal frame crosses the downstream barrier. This prevents a
			// newly opened pane from reusing the id while old output is queued.
			if delivered && route != nil && cl.server != nil {
				cl.server.forgetRoute(route)
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
func (cl *client) authorized() bool {
	select {
	case <-cl.done:
		return false
	default:
	}
	return cl.server == nil || cl.server.clientActive(cl)
}

func (cl *client) writeServer(message muxproto.ServerMsg) error {
	_, err := cl.writeFrame(nil, false, message)
	return err
}

// writePane serializes one frame and binds pane frames to the exact route that
// produced them. Target revocation removes the route before waiting on writeMu:
// a write that already owns the lock completes before the revocation barrier,
// while every later write observes the missing route and is discarded.
func (cl *client) writePane(route *route, message muxproto.ServerMsg) (bool, error) {
	return cl.writeFrame(route, true, message)
}

func (cl *client) writeFrame(route *route, requireAuthorization bool, message muxproto.ServerMsg) (bool, error) {
	cl.writeMu.Lock()
	defer cl.writeMu.Unlock()
	if requireAuthorization && !cl.authorized() {
		return false, net.ErrClosed
	}
	if route != nil && (cl.server == nil || !cl.server.routeCurrent(route)) {
		return false, nil
	}
	if cl.raw != nil {
		if err := cl.raw.SetWriteDeadline(time.Now().Add(downstreamWriteTimeout)); err != nil {
			return false, err
		}
		defer cl.raw.SetWriteDeadline(time.Time{})
	}
	if err := cl.conn.WriteServer(message); err != nil {
		return false, err
	}
	return true, nil
}

func (cl *client) stop() {
	cl.once.Do(func() {
		close(cl.done)
		if cl.conn != nil {
			_ = cl.conn.Close()
		}
	})
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
