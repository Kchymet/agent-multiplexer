// Package daemon is the always-on core of amux. It owns the single poll loop
// over all sources, holds canonical state, serves that state to clients over a
// unix socket, and owns the engine that runs agent processes. Decoupling
// polling from rendering means clients don't each shell out to `claude agents`,
// and because the daemon (not a UI) owns the engine, closing a UI never stops an
// agent.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/panespec"
	"amux/internal/source"
	"amux/internal/wsops"
)

// Daemon polls sources and serves state + actions over a unix socket. It also
// owns the agent engine: agents run inside the daemon-held engine, not the UI,
// so closing or restarting a client never stops them.
type Daemon struct {
	sources  []source.Source
	interval time.Duration
	self     string // absolute path to the amux binary (for spawning rails)
	engine   engine.Engine
	// resolve turns an (agent id, tab) into a launch spec (working dir, env,
	// sandboxed argv). Defaults to panespec.Resolve; overridable in tests.
	resolve func(agentID string, tab int) (dir string, env, argv []string, err error)
	// agentsUnder resolves an id (agent or workgroup root) to the agent ids whose
	// process should run. Defaults to wsops.AgentIDsUnder; overridable in tests.
	agentsUnder func(id string) ([]string, error)

	mu       sync.RWMutex
	sessions []core.Session

	subsMu sync.Mutex
	subs   map[chan core.Snapshot]struct{}

	pollNow chan struct{}
}

// New builds a daemon. self is the absolute path to this binary.
func New(self string, sources []source.Source, interval time.Duration) *Daemon {
	return &Daemon{
		sources:     sources,
		interval:    interval,
		self:        self,
		resolve:     panespec.Resolve,
		agentsUnder: wsops.AgentIDsUnder,
		subs:        map[chan core.Snapshot]struct{}{},
		pollNow:     make(chan struct{}, 1),
	}
}

// Default wires the source set and the local engine. The dashboard is a
// workspace switcher, so the only source is the workspace registry — annotated
// with which agents are running, i.e. "live in the engine".
func Default(self string) *Daemon {
	eng := local.New()
	ws := source.NewWorkspace()
	ws.SetLiveness(func() map[string]bool { return liveAgents(eng) })
	d := New(self, []source.Source{ws}, 2*time.Second)
	d.engine = eng
	return d
}

// liveAgents is the set of agent ids whose agent pane (TabAgent) is running in
// the engine, for the workspace source's liveness annotation.
func liveAgents(eng engine.Engine) map[string]bool {
	m := map[string]bool{}
	for _, k := range eng.Live() {
		if k.Tab == 0 { // TabAgent
			m[k.AgentID] = true
		}
	}
	return m
}

// Run starts the poll loop and socket server until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := os.MkdirAll(core.StateDir(), 0o755); err != nil {
		return err
	}
	sock := core.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	// Clear a stale socket from a previous crash (single-instance is enforced
	// by the caller probing the socket before starting us).
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close(); _ = os.Remove(sock) }()
	// The engine's agents live in this process; stop them cleanly on shutdown.
	// (Agents survive a UI restart — the daemon stays up — but not a daemon
	// restart, e.g. `amux reload`; out-of-process hosting would lift that.)
	if d.engine != nil {
		defer d.engine.Shutdown()
	}

	go d.pollLoop(ctx)

	// Close the listener when ctx ends so Accept returns.
	go func() { <-ctx.Done(); _ = ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(ctx, conn)
	}
}

// ---- polling -------------------------------------------------------------

func (d *Daemon) pollLoop(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	d.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pollOnce(ctx)
		case <-d.pollNow:
			d.pollOnce(ctx)
		}
	}
}

func (d *Daemon) pollOnce(ctx context.Context) {
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all []core.Session
	)
	for _, s := range d.sources {
		wg.Add(1)
		go func(s source.Source) {
			defer wg.Done()
			sess, err := s.Poll(ctx)
			if err != nil {
				log.Printf("poll %s: %v", s.Name(), err)
				return
			}
			mu.Lock()
			all = append(all, sess...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Source != all[j].Source {
			return all[i].Source < all[j].Source
		}
		return all[i].StartedAt < all[j].StartedAt
	})

	d.mu.Lock()
	d.sessions = all
	d.mu.Unlock()
	d.broadcast()
}

func (d *Daemon) snapshot() core.Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	sess := make([]core.Session, len(d.sessions))
	copy(sess, d.sessions)
	return core.Snapshot{Type: "snapshot", Sessions: sess, UpdatedAt: time.Now().UnixMilli()}
}

func (d *Daemon) broadcast() {
	snap := d.snapshot()
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	for ch := range d.subs {
		// Non-blocking: a slow client just misses an intermediate frame.
		select {
		case ch <- snap:
		default:
		}
	}
}

func (d *Daemon) triggerPoll() {
	select {
	case d.pollNow <- struct{}{}:
	default:
	}
}

// ---- connection serving --------------------------------------------------

func (d *Daemon) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	cl := newConnState(conn)
	defer cl.shutdown()

	ch := make(chan core.Snapshot, 4)
	d.subsMu.Lock()
	d.subs[ch] = struct{}{}
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		d.subsMu.Unlock()
	}()

	// Send the current state immediately on connect.
	cl.send(d.snapshot())

	// Push subsequent snapshots.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-cl.done:
				return
			case snap, ok := <-ch:
				if !ok {
					return
				}
				cl.send(snap)
			}
		}
	}()

	// Read actions from the client until it disconnects.
	dec := json.NewDecoder(conn)
	for {
		var a core.Action
		if err := dec.Decode(&a); err != nil {
			return // deferred cl.shutdown detaches panes (without killing agents)
		}
		switch a.Action {
		case core.ActionPaneOpen:
			d.paneOpen(ctx, cl, a)
		case core.ActionPaneInput:
			cl.paneInput(a.PaneID, a.Data)
		case core.ActionPaneResize:
			cl.paneResize(a.PaneID, a.Cols, a.Rows)
		case core.ActionPaneClose:
			cl.paneClose(a.PaneID)
		default:
			res := d.handle(ctx, a)
			if a.Action != "" {
				cl.send(res)
			}
		}
	}
}

// paneOpen attaches this connection to a tab of an agent: it resolves the launch
// spec (working dir, env, sandboxed argv), ensures the engine instance is running
// (starting it on first open, reusing it on reattach), and subscribes the
// connection so the instance replays its scrollback and then streams live output.
func (d *Daemon) paneOpen(ctx context.Context, cl *connState, a core.Action) {
	paneExit := func(msg string) {
		cl.send(core.PaneFrame{Type: core.FramePaneExit, PaneID: a.PaneID, Error: msg})
	}
	if d.engine == nil {
		paneExit("engine unavailable")
		return
	}
	dir, env, argv, err := d.resolve(a.ID, a.Tab)
	if err != nil {
		paneExit(err.Error())
		return
	}
	inst, err := d.engine.Ensure(ctx, engine.Spec{
		Key: engine.Key{AgentID: a.ID, Tab: a.Tab},
		Dir: dir, Env: env, Argv: argv, Cols: a.Cols, Rows: a.Rows,
	})
	if err != nil {
		paneExit(err.Error())
		return
	}
	// Replace any prior subscription on this pane id, then subscribe afresh.
	cl.paneClose(a.PaneID)
	paneID := a.PaneID
	cancel := inst.Subscribe(engine.Sink{
		// Pane output is a stateful byte stream — it must not drop bytes, or the
		// client's emulator corrupts (ghost text). paneOutput coalesces losslessly.
		Output: func(b []byte) { cl.paneOutput(paneID, b) },
		Exit:   func(msg string) { cl.paneExit(paneID, msg) },
	})
	cl.addRoute(paneID, paneRoute{inst: inst, cancel: cancel})
	// Size the instance to this client's viewport (it may have pre-existed at a
	// different size from another client).
	if a.Cols > 0 && a.Rows > 0 {
		inst.Resize(a.Cols, a.Rows)
	}
	d.triggerPoll() // surface the now-live agent in the rail promptly
}

// startEngineFor starts the agent process — the TabAgent pane — for an agent, or
// for every agent under it if id is a workgroup root, without any UI attached.
// This mirrors the TUI, where creating a session and switching to it starts it;
// it lets a CLI-created session come up running in the engine right away. Ensure
// is idempotent, so starting an already-running agent is a no-op.
func (d *Daemon) startEngineFor(ctx context.Context, id string) error {
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	ids, err := d.agentsUnder(id)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("no agent to start for %q", id)
	}
	var firstErr error
	for _, aid := range ids {
		dir, env, argv, err := d.resolve(aid, panespec.TabAgent)
		if err == nil {
			_, err = d.engine.Ensure(ctx, engine.Spec{
				Key: engine.Key{AgentID: aid, Tab: panespec.TabAgent},
				Dir: dir, Env: env, Argv: argv,
			})
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// killEngineFor stops the engine instances (all tabs) of an agent and, if id is a
// workgroup root, its direct children — used when an agent is deleted or
// archived so the process actually stops, not just the store record. It reads
// the current snapshot (still pre-deletion when called from handle) to find
// children.
func (d *Daemon) killEngineFor(id string) {
	if d.engine == nil {
		return
	}
	ids := map[string]bool{id: true}
	d.mu.RLock()
	for _, s := range d.sessions {
		if s.RootID == id {
			ids[s.ID] = true
		}
	}
	d.mu.RUnlock()
	for aid := range ids {
		for tab := 0; tab < 3; tab++ { // agent | editor | terminal
			d.engine.Kill(engine.Key{AgentID: aid, Tab: tab})
		}
	}
}

func (d *Daemon) find(id string) (core.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if s.ID == id {
			return s, true
		}
	}
	return core.Session{}, false
}
