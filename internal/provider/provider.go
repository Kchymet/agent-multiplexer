// Package provider is amux's remote-provider mode: the daemon dials out to a
// remote orchestrator over TLS, registers itself, and serves the harnessproto v2
// protocol over that one long-lived connection — turning this machine into a
// compute node the orchestrator can schedule agent panes onto. It is the
// dial-out counterpart to internal/mux (which listens); it owns PTY-backed panes
// like internal/harness, but the panes outlive the connection so a reconnecting
// orchestrator can adopt them and replay their output losslessly. See
// docs/remote-provider.md for the protocol; amux carries no knowledge of any
// particular orchestrator.
package provider

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"

	"amux/internal/harnessproto"
	"amux/internal/wiretls"
)

// Config is the provider's runtime configuration, assembled by the CLI from
// flags, the AMUX_PROVIDER_* / AMUX_TLS_* env vars, or the config file.
type Config struct {
	Orchestrator string            // address host:port (optionally tls:-prefixed)
	Token        string            // bearer credential
	Name         string            // display name (defaults to hostname upstream)
	Labels       map[string]string // scheduling labels (advisory)
	CAFile       string            // private CA to trust on top of system roots
	ServerName   string            // TLS server-name override (SNI / verification)
	MaxPanes     int               // capability: max concurrent panes
	Features     []string          // capability: opaque feature strings from config

	// Dial, when set, overrides the default TLS dialer (used by tests to run over
	// an in-memory pipe). Production leaves it nil.
	Dial func(context.Context) (net.Conn, error)
	// Logf reports the FSM plainly (dialing, registered, degraded, backoff,
	// terminal errors). Nil discards.
	Logf func(format string, args ...any)
}

// Provider runs provider mode: the reconnect loop and, within each connection,
// the register handshake plus the spawn/input/resize/kill ⇄ output/exit service.
// Panes live on the Provider, not on a session, so they survive disconnects.
type Provider struct {
	cfg      Config
	versions []int

	mu    sync.Mutex
	panes map[string]*pane
	wake  chan struct{} // active session's send-loop wake; nil while disconnected
	grace *time.Timer   // pane-survival deadline while disconnected

	graceDur time.Duration // grace window from the last registered (seconds → duration)

	// Tunables; defaulted by New, overridable by tests.
	backoffMin time.Duration
	backoffMax time.Duration
	graceScale time.Duration // multiplies registered graceSeconds (default 1s)
	hbScale    time.Duration // multiplies registered heartbeatSeconds (default 1s)
}

// New builds a Provider from cfg.
func New(cfg Config) *Provider {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Provider{
		cfg:        cfg,
		versions:   []int{harnessproto.Version, harnessproto.Version2},
		panes:      map[string]*pane{},
		backoffMin: time.Second,
		backoffMax: 30 * time.Second,
		graceScale: time.Second,
		hbScale:    time.Second,
	}
}

// terminalError is a registration rejection the provider must not retry
// (bad-token, revoked, unsupported-version); Run surfaces it and exits.
type terminalError struct{ reason string }

func (e *terminalError) Error() string { return "registration rejected: " + e.reason }

// Run drives the reconnect loop until ctx is cancelled or a terminal
// registration error occurs. Between connections it backs off with jittered
// exponential delay; panes survive disconnects within the grace window. On ctx
// cancellation it kills panes and returns (operator stop).
func (p *Provider) Run(ctx context.Context) error {
	backoff := p.backoffMin
	for {
		if err := ctx.Err(); err != nil {
			p.killAllPanes()
			return err
		}
		conn, err := p.dial(ctx)
		if err != nil {
			p.cfg.Logf("dial %s failed: %v (backoff)", p.cfg.Orchestrator, err)
			if !sleep(ctx, jitter(backoff)) {
				p.killAllPanes()
				return ctx.Err()
			}
			backoff = nextBackoff(backoff, p.backoffMax)
			continue
		}
		registered, err := p.runSession(ctx, conn)
		var te *terminalError
		if asTerminal(err, &te) {
			p.killAllPanes()
			return fmt.Errorf("provider: %w", te)
		}
		if ctx.Err() != nil {
			p.killAllPanes()
			return ctx.Err()
		}
		if registered {
			backoff = p.backoffMin // a healthy connection resets the ladder
		}
		if !sleep(ctx, jitter(backoff)) {
			p.killAllPanes()
			return ctx.Err()
		}
		if !registered {
			backoff = nextBackoff(backoff, p.backoffMax)
		}
	}
}

// dial opens the connection: the injected dialer for tests, else a TLS dial to
// the orchestrator. Provider mode is TLS-only; a tls:-scheme prefix is accepted
// and stripped.
func (p *Provider) dial(ctx context.Context) (net.Conn, error) {
	if p.cfg.Dial != nil {
		return p.cfg.Dial(ctx)
	}
	p.cfg.Logf("dialing %s", p.cfg.Orchestrator)
	return wiretls.DialCA("tcp", stripScheme(p.cfg.Orchestrator), p.cfg.CAFile, p.cfg.ServerName)
}

// session is one connection's transient state: the typed conn, a done channel
// that fans out teardown to the reader/sender/heartbeat goroutines, its own
// send-loop wake, and the last-pong clock for liveness.
type session struct {
	hc       *harnessproto.Conn
	done     chan struct{}
	once     sync.Once
	wake     chan struct{}
	lastPong int64 // atomic UnixNano
}

func (s *session) cancel() { s.once.Do(func() { close(s.done) }) }

// runSession performs the register handshake, then serves the protocol until the
// connection drops or ctx is cancelled. registered reports whether we got a
// successful registered reply (so Run can reset backoff). A terminal registration
// rejection is returned as *terminalError.
func (p *Provider) runSession(ctx context.Context, conn net.Conn) (registered bool, err error) {
	hc := harnessproto.NewConn(conn)
	defer conn.Close()

	// Closing the conn on ctx cancellation unblocks any blocking read/write —
	// including the synchronous registered handshake below, which predates the
	// per-session goroutines and so isn't covered by their done channel.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if werr := hc.WriteHarness(p.registerMsg()); werr != nil {
		return false, nil // couldn't even register; back off and retry
	}
	m, rerr := hc.ReadMux()
	if rerr != nil {
		return false, nil
	}
	if m.Type != harnessproto.MRegistered {
		p.cfg.Logf("expected registered, got %q; dropping", m.Type)
		return false, nil
	}
	if !m.OK {
		switch m.Error {
		case harnessproto.ErrBadToken, harnessproto.ErrRevoked, harnessproto.ErrBadVersion:
			return false, &terminalError{reason: m.Error}
		default:
			p.cfg.Logf("registration rejected: %q; retrying", m.Error)
			return false, nil
		}
	}
	// The orchestrator negotiates the version; confirm it named one we speak, so a
	// bogus/incompatible reply fails loudly rather than silently mis-framing.
	if _, ok := harnessproto.Negotiate([]int{m.Version}, p.versions); !ok {
		return false, &terminalError{reason: harnessproto.ErrBadVersion}
	}

	// Registered: panes are adopted, so cancel the grace countdown from the prior
	// disconnect (if any) and resolve the resume offer.
	p.disarmGrace()
	hb := m.HeartbeatSeconds
	if hb <= 0 {
		hb = 15
	}
	grace := m.GraceSeconds
	if grace <= 0 {
		grace = 60
	}
	p.mu.Lock()
	p.graceDur = time.Duration(grace) * p.graceScale
	p.mu.Unlock()
	p.cfg.Logf("registered: version=%d providerId=%s heartbeat=%ds grace=%ds", m.Version, m.ProviderID, hb, grace)

	sent := p.applyDirectives(m.Adopt, m.Kill)

	s := &session{
		hc:       hc,
		done:     make(chan struct{}),
		wake:     make(chan struct{}, 1),
		lastPong: time.Now().UnixNano(),
	}
	p.mu.Lock()
	p.wake = s.wake
	p.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); p.readLoop(s) }()
	go func() { defer wg.Done(); p.sendLoop(s, sent) }()
	go func() { defer wg.Done(); p.heartbeat(s, hb) }()

	select {
	case <-s.done:
	case <-ctx.Done():
	}
	s.cancel()
	_ = conn.Close() // unblock the reader's ReadMux
	wg.Wait()

	p.mu.Lock()
	if p.wake == s.wake {
		p.wake = nil
	}
	p.mu.Unlock()

	// A dropped connection does not kill panes: arm the grace window so they keep
	// running (and buffering) until we reconnect or grace expires. On operator
	// stop (ctx done) Run kills them instead.
	if ctx.Err() == nil {
		p.armGrace()
		p.cfg.Logf("disconnected; panes survive for grace window, reconnecting")
	}
	return true, nil
}

// registerMsg builds the first frame: offered versions, credential, identity,
// capabilities, and resume offers for surviving panes.
func (p *Provider) registerMsg() harnessproto.HarnessMsg {
	return harnessproto.HarnessMsg{
		Type:         harnessproto.HRegister,
		Versions:     p.versions,
		Token:        p.cfg.Token,
		Name:         p.cfg.Name,
		Labels:       p.cfg.Labels,
		Capabilities: p.capabilities(),
		Panes:        p.paneOffers(),
	}
}

// capabilities advertises what this machine can run. bwrap is probed for; os/arch
// are the build target; features are opaque config strings (never hardcoded here —
// orchestrators match on them by convention).
func (p *Provider) capabilities() *harnessproto.Capabilities {
	_, err := exec.LookPath("bwrap")
	return &harnessproto.Capabilities{
		MaxPanes: p.cfg.MaxPanes,
		Bwrap:    err == nil,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Features: p.cfg.Features,
	}
}

// paneOffers lists surviving panes for the register resume offer.
func (p *Provider) paneOffers() []harnessproto.PaneOffer {
	p.mu.Lock()
	defer p.mu.Unlock()
	var offers []harnessproto.PaneOffer
	for id, pn := range p.panes {
		seq, exited := pn.buf.snapshot()
		offers = append(offers, harnessproto.PaneOffer{PaneID: id, OutSeq: seq, Running: !exited})
	}
	return offers
}

// applyDirectives resolves the resume offer: adopted panes get a send cursor at
// their afterSeq (so output replays from there); every other surviving pane is
// killed (the orchestrator either listed it under kill or omitted it, both of
// which mean terminate).
func (p *Provider) applyDirectives(adopt []harnessproto.AdoptPane, _ []string) map[string]int64 {
	sent := map[string]int64{}
	p.mu.Lock()
	defer p.mu.Unlock()
	adopted := map[string]bool{}
	for _, a := range adopt {
		if _, ok := p.panes[a.PaneID]; ok {
			sent[a.PaneID] = a.AfterSeq
			adopted[a.PaneID] = true
		}
	}
	for id, pn := range p.panes {
		if !adopted[id] {
			pn.terminate()
			delete(p.panes, id)
		}
	}
	return sent
}

// ---- protocol goroutines ----

// readLoop handles orchestrator → provider frames until the connection errors.
func (p *Provider) readLoop(s *session) {
	for {
		m, err := s.hc.ReadMux()
		if err != nil {
			s.cancel()
			return
		}
		switch m.Type {
		case harnessproto.MSpawn:
			p.spawn(m)
		case harnessproto.MInput:
			if pn := p.getPane(m.PaneID); pn != nil && pn.ptmx != nil {
				_, _ = pn.ptmx.Write(m.Data)
			}
		case harnessproto.MResize:
			if pn := p.getPane(m.PaneID); pn != nil && pn.ptmx != nil && m.Cols > 0 && m.Rows > 0 {
				_ = pty.Setsize(pn.ptmx, &pty.Winsize{Cols: uint16(m.Cols), Rows: uint16(m.Rows)})
			}
		case harnessproto.MKill:
			p.mu.Lock()
			p.killLocked(m.PaneID)
			p.mu.Unlock()
		case harnessproto.MPong:
			atomic.StoreInt64(&s.lastPong, time.Now().UnixNano())
		}
	}
}

// sendLoop drains pane replay buffers to the connection in seq order, waking on
// pane output (or a spawn). sent tracks the last seq delivered per pane; a slow
// or reconnecting reader whose next frame was trimmed gets a reset first. A write
// error tears the session down.
func (p *Provider) sendLoop(s *session, sent map[string]int64) {
	for {
		if err := p.drainAll(s.hc, sent); err != nil {
			s.cancel()
			return
		}
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
	}
}

// drainAll flushes every pane's pending frames once. It snapshots the pane set
// under the lock, then copies and writes each pane's frames without holding it,
// so a blocking socket never stalls the PTY pumps. A pane whose exit frame is
// delivered here is forgotten (the orchestrator has seen the terminal frame).
func (p *Provider) drainAll(hc *harnessproto.Conn, sent map[string]int64) error {
	p.mu.Lock()
	type entry struct {
		id  string
		buf *paneBuf
	}
	list := make([]entry, 0, len(p.panes))
	for id, pn := range p.panes {
		list = append(list, entry{id, pn.buf})
	}
	p.mu.Unlock()

	var remove []string
	for _, e := range list {
		needReset, resetSeq, frames, last := e.buf.framesAfter(sent[e.id])
		if needReset {
			if err := hc.WriteHarness(harnessproto.HarnessMsg{Type: harnessproto.HReset, PaneID: e.id, Seq: resetSeq}); err != nil {
				return err
			}
		}
		exited := false
		for _, f := range frames {
			var msg harnessproto.HarnessMsg
			if f.kind == frameExit {
				msg = harnessproto.HarnessMsg{Type: harnessproto.HExit, PaneID: e.id, Error: f.err, Seq: f.seq}
				exited = true
			} else {
				msg = harnessproto.HarnessMsg{Type: harnessproto.HOutput, PaneID: e.id, Data: f.data, Seq: f.seq}
			}
			if err := hc.WriteHarness(msg); err != nil {
				return err
			}
		}
		sent[e.id] = last
		if exited {
			remove = append(remove, e.id)
		}
	}
	if len(remove) > 0 {
		p.mu.Lock()
		for _, id := range remove {
			delete(p.panes, id)
			delete(sent, id)
		}
		p.mu.Unlock()
	}
	return nil
}

// heartbeat pings at the orchestrator's cadence and declares the connection dead
// after 4 missed intervals without a pong (per spec).
func (p *Provider) heartbeat(s *session, hbSec int) {
	d := time.Duration(hbSec) * p.hbScale
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			last := atomic.LoadInt64(&s.lastPong)
			if time.Since(time.Unix(0, last)) > 4*d {
				p.cfg.Logf("no pong within %s; connection dead", 4*d)
				s.cancel()
				return
			}
			if err := s.hc.WriteHarness(harnessproto.HarnessMsg{Type: harnessproto.HPing, T: time.Now().UnixNano()}); err != nil {
				s.cancel()
				return
			}
		}
	}
}

// ---- pane lifecycle ----

// spawn starts a PTY-backed process for a pane and begins pumping its output into
// the replay buffer. Mirrors internal/harness: the provider supplies the local
// execution environment (PATH etc., with TMUX/TERM stripped for a fresh
// terminal); the orchestrator supplies the workload-specific vars in m.Env.
func (p *Provider) spawn(m harnessproto.MuxMsg) {
	if len(m.Argv) == 0 {
		p.addExitedPane(m.PaneID, "empty argv")
		return
	}
	cmd := exec.Command(m.Argv[0], m.Argv[1:]...)
	cmd.Dir = m.Dir
	cmd.Env = append(stripEnv(os.Environ(), "TMUX", "TERM"), m.Env...)
	cols, rows := m.Cols, m.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		p.addExitedPane(m.PaneID, err.Error())
		return
	}
	pn := &pane{ptmx: ptmx, cmd: cmd, buf: &paneBuf{}}
	p.mu.Lock()
	p.panes[m.PaneID] = pn
	p.mu.Unlock()
	go p.pump(m.PaneID, pn)
}

// addExitedPane records a pane that failed before (or without) starting a
// process, so the orchestrator still receives an exit frame.
func (p *Provider) addExitedPane(id, errMsg string) {
	pn := &pane{buf: &paneBuf{}}
	pn.buf.appendExit(errMsg)
	p.mu.Lock()
	p.panes[id] = pn
	p.mu.Unlock()
	p.signal()
}

// pump streams a pane's PTY output into its replay buffer until EOF, then waits
// the process and appends the exit frame. It never removes the pane — that
// happens when the exit frame is delivered (drainAll), the orchestrator kills it,
// or grace expires — so the exit survives a disconnect and replays on adopt.
func (p *Provider) pump(id string, pn *pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := pn.ptmx.Read(buf)
		if n > 0 {
			pn.buf.appendOutput(buf[:n])
			p.signal()
		}
		if err != nil {
			werr := ""
			if e := pn.cmd.Wait(); e != nil {
				werr = e.Error()
			}
			_ = pn.ptmx.Close()
			pn.buf.appendExit(werr)
			p.signal()
			return
		}
	}
}

func (p *Provider) getPane(id string) *pane {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.panes[id]
}

// signal wakes the active session's send loop, if any. Coalesced: one nudge
// covers any number of pending appends. A no-op while disconnected.
func (p *Provider) signal() {
	p.mu.Lock()
	w := p.wake
	p.mu.Unlock()
	if w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// killLocked terminates a pane and forgets it. Caller holds p.mu.
func (p *Provider) killLocked(id string) {
	if pn := p.panes[id]; pn != nil {
		delete(p.panes, id)
		pn.terminate()
	}
}

// killAllPanes terminates every pane and clears the grace timer. Used on grace
// expiry and on operator stop.
func (p *Provider) killAllPanes() {
	p.mu.Lock()
	ps := p.panes
	p.panes = map[string]*pane{}
	if p.grace != nil {
		p.grace.Stop()
		p.grace = nil
	}
	p.mu.Unlock()
	for _, pn := range ps {
		pn.terminate()
	}
	if len(ps) > 0 {
		p.cfg.Logf("killed %d pane(s)", len(ps))
	}
}

// armGrace starts the pane-survival deadline on disconnect (idempotent while
// running). On expiry, orchestrator-owned panes are killed and buffers discarded.
func (p *Provider) armGrace() {
	p.mu.Lock()
	d := p.graceDur
	if d <= 0 {
		d = 60 * p.graceScale
	}
	if p.grace == nil {
		p.grace = time.AfterFunc(d, p.killAllPanes)
	}
	p.mu.Unlock()
}

// disarmGrace cancels the survival deadline (a reconnect adopted the panes).
func (p *Provider) disarmGrace() {
	p.mu.Lock()
	if p.grace != nil {
		p.grace.Stop()
		p.grace = nil
	}
	p.mu.Unlock()
}

// ---- helpers ----

// stripEnv removes any KEY=... entries for the given keys (see internal/harness).
func stripEnv(env []string, keys ...string) []string {
	out := env[:0:0]
	for _, e := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

// stripScheme drops a leading tls: / tls:// from an orchestrator address.
func stripScheme(addr string) string {
	return strings.TrimPrefix(strings.TrimPrefix(addr, "tls:"), "//")
}

// nextBackoff doubles d up to the cap.
func nextBackoff(d, cap time.Duration) time.Duration {
	d *= 2
	if d > cap {
		d = cap
	}
	return d
}

// jitter spreads a backoff delay over [d/2, d] to avoid synchronized reconnects.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// sleep waits for d or ctx cancellation; it reports false if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// asTerminal reports whether err is a *terminalError, binding it into dst.
func asTerminal(err error, dst **terminalError) bool {
	te, ok := err.(*terminalError)
	if ok {
		*dst = te
	}
	return ok
}
