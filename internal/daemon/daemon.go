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

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/panespec"
	"amux/internal/source"
	"amux/internal/store"
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

	// liveAgentsPath is where the live engine-instance set is persisted so a
	// restart can relaunch it. Defaults to core.LiveAgentsPath(); overridable in
	// tests.
	liveAgentsPath string
	persistMu      sync.Mutex
	lastLive       string // marshaled last-written set, to skip redundant writes

	// pendingRestore holds the live set read from disk at startup, relaunched
	// once after the first poll resolves sessions/specs.
	pendingRestore []engine.Key

	// firstPoll is closed after the first pollOnce completes, so restore waits
	// until sessions/specs are resolvable.
	firstPoll     chan struct{}
	firstPollOnce sync.Once
}

// New builds a daemon. self is the absolute path to this binary.
func New(self string, sources []source.Source, interval time.Duration) *Daemon {
	return &Daemon{
		sources:        sources,
		interval:       interval,
		self:           self,
		resolve:        panespec.Resolve,
		agentsUnder:    wsops.AgentIDsUnder,
		subs:           map[chan core.Snapshot]struct{}{},
		pollNow:        make(chan struct{}, 1),
		liveAgentsPath: core.LiveAgentsPath(),
		firstPoll:      make(chan struct{}),
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
	// Let the engine defer terminating a mid-turn agent on graceful shutdown until
	// it is safe. The probe closes over d, so it must be set after d is built.
	eng.SetActivity(d.instanceActivity)
	return d
}

// instanceActivity reports an engine instance's turn state for the engine's
// graceful-shutdown wait, routed through the abstract agent.Harness so the engine
// never reads Claude's hooks directly. It resolves the instance's agent id to its
// harness session id and kind via the store, then asks that kind's harness. It is
// best-effort: any error/not-found yields ActivityUnknown, which the engine
// treats as safe to stop, so a missing signal never blocks a shutdown.
func (d *Daemon) instanceActivity(k engine.Key) engine.Activity {
	db, err := store.Open()
	if err != nil {
		return engine.ActivityUnknown
	}
	defer db.Close()
	s, ok, err := db.GetSession(k.AgentID)
	if err != nil || !ok {
		return engine.ActivityUnknown
	}
	kind := s.Agent
	if kind == "" {
		kind = "claude"
	}
	return agent.HarnessFor(kind).Activity(s.ClaudeID)
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
	// Persist the live set before killing them, so the next startup relaunches it.
	if d.engine != nil {
		defer func() {
			d.persistLiveAgents()
			d.engine.Shutdown()
		}()
	}

	// Read the previously-live set BEFORE any poll persists over the file, then
	// relaunch it once sessions/specs are resolvable (after the first poll).
	d.pendingRestore = d.readLiveAgents()
	go func() {
		select {
		case <-ctx.Done():
		case <-d.firstPoll:
			d.restoreLiveAgents(ctx)
		}
	}()

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

	// Snapshot the live engine set each poll so a crash leaves a recent record;
	// then release restore, which waited for sessions/specs to resolve.
	d.persistLiveAgents()
	d.firstPollOnce.Do(func() { close(d.firstPoll) })
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
		case core.ActionQuery:
			d.query(cl, a)
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

// persistLiveAgents writes the current set of live engine keys to disk so a
// daemon restart can relaunch them without a UI trigger. It skips the write when
// the set is unchanged from the last one, so the per-poll call is cheap. Failures
// are logged, never fatal — persistence is best-effort.
func (d *Daemon) persistLiveAgents() {
	if d.engine == nil || d.liveAgentsPath == "" {
		return
	}
	keys := d.engine.Live()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].AgentID != keys[j].AgentID {
			return keys[i].AgentID < keys[j].AgentID
		}
		return keys[i].Tab < keys[j].Tab
	})
	buf, err := json.Marshal(keys)
	if err != nil {
		return
	}
	d.persistMu.Lock()
	defer d.persistMu.Unlock()
	if string(buf) == d.lastLive {
		return
	}
	// Write atomically so a crash mid-write can't leave a truncated file.
	tmp := d.liveAgentsPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		log.Printf("persist live agents: %v", err)
		return
	}
	if err := os.Rename(tmp, d.liveAgentsPath); err != nil {
		log.Printf("persist live agents: %v", err)
		return
	}
	d.lastLive = string(buf)
}

// readLiveAgents reads the persisted live set. A missing/garbage file yields nil
// (nothing to restore), never an error — restore is best-effort.
func (d *Daemon) readLiveAgents() []engine.Key {
	if d.liveAgentsPath == "" {
		return nil
	}
	buf, err := os.ReadFile(d.liveAgentsPath)
	if err != nil {
		return nil
	}
	var keys []engine.Key
	if err := json.Unmarshal(buf, &keys); err != nil {
		log.Printf("read live agents: %v", err)
		return nil
	}
	return keys
}

// restoreLiveAgents relaunches the agent panes that were live when the daemon
// last stopped, headlessly (no subscriber attached — the engine buffers output in
// its scrollback ring until a UI attaches). It restores only the agent tab
// (TabAgent), and only agents that still exist and aren't archived, so it never
// resurrects deleted/archived sessions or auto-starts editor/shell tabs. Gated by
// $AMUX_RESTORE (default on). Ensure is idempotent, so a race with any other
// starter is harmless.
func (d *Daemon) restoreLiveAgents(ctx context.Context) {
	if v := os.Getenv("AMUX_RESTORE"); v == "0" || v == "false" {
		return
	}
	restored := 0
	for _, k := range d.pendingRestore {
		if k.Tab != panespec.TabAgent {
			continue // only the agent process is auto-restored, not editor/shell
		}
		if !d.restorable(k.AgentID) {
			continue // deleted or archived while the daemon was down
		}
		if err := d.startEngineFor(ctx, k.AgentID); err != nil {
			log.Printf("restore agent %s: %v", k.AgentID, err)
			continue
		}
		restored++
	}
	if restored > 0 {
		log.Printf("restored %d agent(s) from the previous run", restored)
		d.triggerPoll() // surface the now-live agents in the rail
	}
}

// restorable reports whether an agent id is present in the current snapshot and
// not archived — i.e. safe to relaunch on restore.
func (d *Daemon) restorable(agentID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if s.ID == agentID {
			return s.Section != core.SectionArchived
		}
	}
	return false
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
