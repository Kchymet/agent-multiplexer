// Package mux is amux's multiplexer server: the backend half of the client/server
// split. It speaks muxproto to authenticated UI clients, relays authoritative
// state/control to the primary daemon, and routes agent pane I/O between clients
// and an agent harness (harnessproto). See
// docs/client-server.md.
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
	"amux/internal/harness"
	"amux/internal/muxproto"
	"amux/internal/panespec"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Server is a running multiplexer server.
type Server struct {
	primary Primary
	token   string // nonempty bearer required inside an authenticated TLS channel

	mu           sync.Mutex
	clients      map[*client]bool
	routes       map[string]route // harness pane id -> owning client + its pane id
	hconn        *harnessproto.Conn
	paneSeq      int64
	lastSnap     []byte // last broadcast snapshot, for change detection
	lastSessions []core.Session

	pollCh chan struct{}

	// launchSpec is injected by the daemon/provider integration that owns the
	// current session record and access authority. The legacy mux must not derive
	// identity or provision a second authority from an outer pane-open message.
	launchSpec LaunchSpecResolver
	resolve    paneResolver
}

type LaunchSpecResolver func(context.Context, string) (panespec.LaunchSpec, error)
type paneResolver func(panespec.LaunchSpec, int) (dir string, env, argv []string, err error)

// Primary is the mux's only state/authority seam. Production implements it by
// authenticating to the singleton daemon as the host for every operation. Tests
// inject inert fakes; no mux path opens the store or a FileAuthority.
type Primary interface {
	Snapshot(context.Context) ([]core.Session, error)
	Dispatch(context.Context, core.Action) (string, error)
	LaunchSpec(context.Context, string) (panespec.LaunchSpec, error)
}

type unavailablePrimary struct{}

func (unavailablePrimary) Snapshot(context.Context) ([]core.Session, error) {
	return nil, fmt.Errorf("legacy mux has no authenticated primary daemon relay")
}
func (unavailablePrimary) Dispatch(context.Context, core.Action) (string, error) {
	return "", fmt.Errorf("legacy mux has no authenticated primary daemon relay")
}
func (unavailablePrimary) LaunchSpec(context.Context, string) (panespec.LaunchSpec, error) {
	return panespec.LaunchSpec{}, fmt.Errorf("legacy mux has no daemon-authorized launch resolver")
}

type route struct {
	cl         *client
	clientPane string
	agent      string // agent id this pane belongs to, so a delete/archive can find it
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
	primaryReadTimeout   = 5 * time.Second
	primaryActionTimeout = 30 * time.Second
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
		primary:    primary,
		token:      os.Getenv("AMUX_MUX_TOKEN"),
		clients:    map[*client]bool{},
		routes:     map[string]route{},
		pollCh:     make(chan struct{}, 1),
		launchSpec: primary.LaunchSpec,
		resolve:    panespec.Resolve,
	}
}

// Serve starts the harness and the poll loop, then accepts clients on every
// listener until ctx is cancelled. Blocks.
func (s *Server) Serve(ctx context.Context, lns ...net.Listener) error {
	if err := s.startHarness(); err != nil {
		return err
	}
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

// ---- harness (in-process over net.Pipe; the protocol is real either way) ----

func (s *Server) startHarness() error {
	a, b := net.Pipe()
	s.hconn = harnessproto.NewConn(a)
	go func() { _ = harness.Serve(harnessproto.NewConn(b)) }()
	r, err := s.hconn.ReadHarness()
	if err != nil {
		_ = s.hconn.Close()
		return fmt.Errorf("start embedded harness: %w", err)
	}
	if r.Type != harnessproto.HReady {
		_ = s.hconn.Close()
		return fmt.Errorf("start embedded harness: unexpected ready frame %q", r.Type)
	}
	go s.readHarness()
	return nil
}

// readHarness routes harness output/exit frames to the client that owns the pane.
func (s *Server) readHarness() {
	for {
		m, err := s.hconn.ReadHarness()
		if err != nil {
			s.suspend()
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
	authCtx, cancelAuth := context.WithTimeout(context.Background(), primaryReadTimeout)
	sessions, err := s.primary.Snapshot(authCtx)
	cancelAuth()
	if err != nil {
		cl.reject(muxproto.ErrUnauthorized)
		_ = cl.conn.Close()
		return
	}
	s.mu.Lock()
	s.clients[cl] = true
	s.rememberLocked(sessions)
	s.mu.Unlock()
	host, _ := os.Hostname()
	if err := cl.conn.WriteServer(muxproto.ServerMsg{Type: muxproto.SWelcome, OK: true, Version: muxproto.Version, Server: host}); err != nil {
		s.dropClient(cl)
		return
	}
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
		if err == nil && s.rememberFor(cl, sess) {
			s.mu.Lock()
			cl.sub = true
			s.mu.Unlock()
			cl.send(muxproto.ServerMsg{Type: muxproto.SSnapshot, Sessions: sess})
		} else if err != nil {
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
		before := s.sessions()
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
		if err == nil && core.DescriptorFor(act.Action).StopsEngine {
			s.killPanesFor(sessionIDsUnder(before, act.ID))
		}
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
	_, duplicate := cl.panes[m.PaneID]
	s.mu.Unlock()
	if duplicate {
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: "pane id already open"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), primaryActionTimeout)
	spec, err := s.launchSpec(ctx, m.Agent)
	cancel()
	if err != nil {
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: err.Error()})
		return
	}
	if !s.clientActive(cl) {
		return
	}
	dir, env, argv, err := s.resolve(spec, m.Tab)
	if err != nil {
		cl.send(muxproto.ServerMsg{Type: muxproto.SPaneExit, PaneID: m.PaneID, Error: err.Error()})
		return
	}
	env = append(env, "TERM=xterm-256color")
	s.mu.Lock()
	if !s.clients[cl] {
		s.mu.Unlock()
		return
	}
	s.paneSeq++
	hp := "h" + itoa(s.paneSeq)
	s.routes[hp] = route{cl: cl, clientPane: m.PaneID, agent: m.Agent}
	cl.panes[m.PaneID] = hp
	s.mu.Unlock()
	if err := s.hconn.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: hp, Dir: dir, Env: env, Argv: argv, Cols: m.Cols, Rows: m.Rows,
	}); err != nil {
		s.suspend()
	}
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

// killPanesFor tears down the selected live panes after the primary daemon has
// accepted a StopsEngine action. Workgroup membership is derived from the cached
// pre-action authoritative snapshot, never from a local store read. Sending
// MKill is enough: the harness answers with HExit through the normal cleanup path.
func (s *Server) killPanesFor(ids []string) {
	if len(ids) == 0 {
		return
	}
	want := make(map[string]bool, len(ids))
	for _, a := range ids {
		want[a] = true
	}
	s.mu.Lock()
	var kill []string
	for hp, r := range s.routes {
		if want[r.agent] {
			kill = append(kill, hp)
		}
	}
	s.mu.Unlock()
	for _, hp := range kill {
		_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MKill, PaneID: hp})
	}
}

func sessionIDsUnder(sessions []core.Session, id string) []string {
	if id == "" {
		return nil
	}
	ids := []string{id}
	for _, session := range sessions {
		if session.RootID == id && session.ID != id {
			ids = append(ids, session.ID)
		}
	}
	return ids
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
	s.lastSnap = b
	s.lastSessions = append(s.lastSessions[:0], sess...)
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

// suspend revokes every cached grant when the primary daemon can no longer be
// authenticated/read. Existing clients are disconnected and every mux-owned
// pane is killed; a later successful poll does not resurrect either. This makes
// the one-second poll interval an explicit maximum freshness lease instead of
// retaining host authority indefinitely through a daemon failure/revocation.
func (s *Server) suspend() {
	s.mu.Lock()
	clients := make([]*client, 0, len(s.clients))
	for cl := range s.clients {
		clients = append(clients, cl)
		cl.panes = map[string]string{}
	}
	panes := make([]string, 0, len(s.routes))
	for pane := range s.routes {
		panes = append(panes, pane)
	}
	s.clients = map[*client]bool{}
	s.routes = map[string]route{}
	s.lastSnap = nil
	s.lastSessions = nil
	s.mu.Unlock()
	for _, cl := range clients {
		cl.stop()
	}
	for _, pane := range panes {
		if s.hconn != nil {
			_ = s.hconn.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MKill, PaneID: pane})
		}
	}
}

func (s *Server) remember(sessions []core.Session) {
	s.mu.Lock()
	s.rememberLocked(sessions)
	s.mu.Unlock()
}

func (s *Server) rememberLocked(sessions []core.Session) {
	b, _ := json.Marshal(sessions)
	s.lastSnap = b
	s.lastSessions = append(s.lastSessions[:0], sessions...)
}

func (s *Server) rememberFor(cl *client, sessions []core.Session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.clients[cl] {
		return false
	}
	s.rememberLocked(sessions)
	return true
}

func (s *Server) clientActive(cl *client) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clients[cl]
}

func (s *Server) sessions() []core.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Session(nil), s.lastSessions...)
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
