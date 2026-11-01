// Package daemon is the always-on core of amux. It owns the single poll loop
// over all sources, holds canonical state, serves that state to clients over a
// unix socket, and owns the engine that runs agent processes. Decoupling
// polling from rendering means clients don't each shell out to `claude agents`,
// and because the daemon (not a UI) owns the engine, closing a UI never stops an
// agent.
package daemon

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/claudecfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/daemonstatus"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/procstat"
	"amux/internal/prwatch"
	"amux/internal/source"
	"amux/internal/store"
	"amux/internal/wsops"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Daemon polls sources and serves state + actions over a unix socket. It also
// owns the agent engine: agents run inside the daemon-held engine, not the UI,
// so closing or restarting a client never stops them.
type Daemon struct {
	// resources is the background process sampler behind QueryResources (nil
	// until Run starts it, or when sampling is off; resourcesOff says why).
	resourcesMu  sync.Mutex
	resources    *procstat.Sampler
	resourcesOff string

	sources  []source.Source
	interval time.Duration
	self     string // absolute path to the amux binary (for spawning rails)
	engine   engine.Engine
	// authority is initialized before the singleton socket is touched. Every
	// connection authenticates against it before connState/subscription exists.
	authority *access.FileAuthority
	// resolve turns an (agent id, tab) into a launch spec (working dir, env,
	// sandboxed argv). Defaults to panespec.Resolve; overridable in tests.
	resolve launchResolver
	// launchSpec captures the authoritative session row and access grant
	// together. It is injectable only for isolated engine tests.
	launchSpec launchSpecResolver
	// listen publishes the authenticated host listener. Production uses
	// listenOwnedUnix; lifecycle tests inject a blocking in-memory listener so
	// shutdown ordering does not require a host socket.
	listen func(string, string) (net.Listener, func(), error)
	// agentsUnder resolves an id (agent or workgroup root) to the agent ids whose
	// process should run. Defaults to wsops.AgentIDsUnder; overridable in tests.
	agentsUnder func(id string) ([]string, error)
	// dispatch applies a store-backed host action (wsops.Dispatch); tests
	// override it to stall inside an action's own work.
	dispatch func(ctx context.Context, a core.Action, stopEngine func(id string)) (string, error)

	// codex supervises structured-control Codex sessions (AGE-181): a background
	// App Server per session, owned by the daemon, independent of any pane. It is
	// created in Run (bound to the daemon context) and is inert until a session is
	// launched with App Server control enabled, so the default PTY path is
	// unaffected. nil in tests that don't exercise structured control.
	codex *codexapp.Manager
	// centralCodex owns the central Codex home when codex.auth is central.
	centralCodex *centralCodex
	accountMu    sync.Mutex
	// accountLocks orders one account profile's sign-in steps (admitAccount).
	accountLocks keyedLocks
	// beforeAccountStep, when set, runs inside an account operation's
	// admission; tests stall it there.
	beforeAccountStep func(profileID string)
	// readiness is the last account snapshot, reused by creation preflight.
	readiness       accountReadinessCache
	accountAttempts map[string]*accountAttempt
	accountNow      func() time.Time
	// claudeUsage reads a Claude usage report; nil uses the vendor API.
	claudeUsage func(context.Context, string) (claudeUsageReport, error)
	// claudeProbe reads a setup-token's windows; nil sends the quota probe.
	claudeProbe func(context.Context, string) ([]harnessproto.AccountLimitWindow, error)
	// claudeLogin reads a host or shared Claude login; nil reads its store.
	claudeLogin func(mode string) (claudecfg.OAuthLogin, error)
	// hostCodexLimits reads the host Codex login's windows; nil runs codex.
	hostCodexLimits func(context.Context) (harnessproto.AccountLimits, error)
	// claudePaused fingerprints the credential each profile's persisted
	// RetryAt pause applies to. Only the account limits loop touches it.
	claudePaused map[string][32]byte
	// claudeNoUsage fingerprints each central credential the usage report
	// refused for scope; those profiles are probed instead.
	claudeNoUsage map[string][32]byte

	// Captured once at construction; config edits never reroute a running daemon.
	codexControl amuxcfg.Control
	configErr    error
	// goalAutoResume is the goal.auto_resume switch (amuxcfg): whether a
	// restarted Claude goal coordinator continues its eligible goal without a
	// user turn (restartIntent). Captured once, like codexControl.
	goalAutoResume bool

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
	pendingRestore []restartRecord

	// steerSettle is how old a runtime must be before typed steering text reaches
	// it, so its TUI has painted a composer. Zero means steerStartSettle;
	// tests set it small so they don't sleep.
	steerSettle time.Duration

	// steerStarted, when non-nil, receives the session id of each deferred start
	// behind an accepted `prompt` once it has finished, successfully or not.
	// Production leaves it nil — the session journal is the report — and tests set
	// a buffered channel to join work that deliberately outlives the verb that
	// asked for it. The send never blocks the start.
	steerStarted chan string

	// steerAdmission, when non-nil, runs after a deferred steering admission is
	// bounded but before it takes the final effect locks. Production leaves it
	// nil; tests use it to hold accepted work beyond its durable response.
	steerAdmission func(context.Context) error

	// firstPoll is closed after the first pollOnce completes, so restore waits
	// until sessions/specs are resolvable.
	firstPoll     chan struct{}
	firstPollOnce sync.Once

	// authPending contains only the instances live when the user requested a
	// credential reload. New/replacement instances already inherit current auth.
	authMu      sync.Mutex
	authPending map[engine.Key]authReload

	// effectMu is the final principal/policy/effect admission boundary shared by
	// authenticated host actions, session mailbox dispatch, credential rotation,
	// and completion revocation. A policy mutation cannot commit between a
	// session's last current-generation check and admission of its exact effect.
	// Holders not yet split per resource take it exclusively; session-scoped
	// work takes it shared plus sessionLocks (admitSession). See
	// docs/daemon-locking.md.
	effectMu sync.RWMutex
	// sessionLocks orders effects on one session's own state.
	sessionLocks keyedLocks
	// creationMu serializes session creations among themselves, under the
	// shared gate. Reconcile opens a new mailbox only while it can hold it, so
	// it never adopts a half-created session. Order: gate, session locks, then
	// creationMu; a creation releases it before locking its new session.
	creationMu sync.Mutex
	// credentialMu serializes host credential helper processes (keychain, gh)
	// among themselves. It is a leaf below a session's admission.
	credentialMu sync.Mutex

	// shutdown is closed only by the authenticated host control path. Process
	// IDs are diagnostics, never authority to signal a process.
	shutdown     chan struct{}
	shutdownOnce sync.Once

	// serving tracks every accepted host transport. Run closes and joins these
	// handlers before it closes the session RPC servers or releases authority
	// ownership. That ordering matters for blocked TLS writes and for handlers
	// whose invalidation watcher selected ctx.Done during shutdown.
	servingMu       sync.Mutex
	serving         map[uint64]net.Conn
	servingNext     uint64
	servingDraining bool
	servingWG       sync.WaitGroup

	// deferred owns asynchronous work accepted by a control action after its
	// immediate response is committed (cold starts and structured prompts).
	// Shutdown closes admission before waiting, so WaitGroup.Add can never race
	// Wait and no acknowledged effect can outlive the engines or authority it
	// may still use.
	deferredMu       sync.Mutex
	deferredDraining bool
	deferredWG       sync.WaitGroup

	// permissions owns runtime-generation binding and atomic request consumption
	// for every caller role. It is initialized even in tests that do not Run.
	permissions *runtimePermissionGate
	// permissionBaseline records unresolved durable requests before a newly
	// observed runtime is assigned a generation. Tests inject a record-free
	// resolver; production reads through the daemon's runtime-record seam.
	permissionBaseline func(string) ([]string, error)
	// sessionRPC owns bounded per-session mailbox serving and lifecycle hooks.
	sessionRPC *sessionRuntime
	// prs watches workgroup members' GitHub PRs and wakes their coordinators
	// (internal/prwatch, prwatch.go). Built in New, started by Run.
	prs *prwatch.Watcher
	// prWakeAdmission runs after a watcher delivery holds its final membership,
	// repo, and coordinator admission. Production leaves it nil; tests use it to
	// prove mutations cannot cross the authority recheck and prompt boundary.
	prWakeAdmission func()
	// status publishes startup/shutdown progress for lifecycle clients. Run
	// creates it only once it holds the singleton lock, which owns the file.
	status *daemonstatus.Reporter
}

type launchResolver func(panespec.LaunchSpec, int) (dir string, env, argv []string, err error)
type launchSpecResolver func(context.Context, string) (panespec.LaunchSpec, error)

// New builds a daemon and captures its Codex control selection. Run reports
// any configuration error before opening a socket. self is this binary's path.
func New(self string, sources []source.Source, interval time.Duration) *Daemon {
	control, err := amuxcfg.ResolveCodexControl()
	goalAutoResume, goalErr := amuxcfg.ResolveGoalAutoResume()
	d := &Daemon{
		codexControl:   control,
		configErr:      errors.Join(err, goalErr),
		goalAutoResume: goalAutoResume,
		sources:        sources,
		interval:       interval,
		self:           self,
		resolve:        panespec.Resolve,
		agentsUnder:    wsops.AgentIDsUnder,
		dispatch:       wsops.Dispatch,
		subs:           map[chan core.Snapshot]struct{}{},
		pollNow:        make(chan struct{}, 1),
		liveAgentsPath: core.LiveAgentsPath(),
		firstPoll:      make(chan struct{}),
		shutdown:       make(chan struct{}),
		serving:        make(map[uint64]net.Conn),
		permissions:    newRuntimePermissionGate(),
	}
	d.launchSpec = d.launchSpecFor
	d.listen = listenOwnedUnix
	d.prs = d.newPRWatcher()
	d.permissionBaseline = d.loadPermissionBaseline
	return d
}

// Default wires the source set and the local engine. The dashboard is a
// workspace switcher, so the only source is the workspace registry — annotated
// with which agents are running, i.e. "live in the engine".
func Default(self string) *Daemon {
	// The long-lived daemon opens the store on every reconcile tick and several
	// times per session report; reuse pooled connections instead of paying
	// SQLite connection setup and schema parsing each time.
	store.ShareHandles(true)
	eng := local.New()
	ws := source.NewWorkspace()
	d := New(self, []source.Source{ws}, 2*time.Second)
	d.engine = eng
	// Liveness is an agent running in the engine (a PTY pane) OR under the App
	// Server supervisor (a structured session has no pane). Both are read at call
	// time, so the probe works once Run installs the manager.
	ws.SetLiveness(func() map[string]bool {
		m := liveAgents(eng)
		if d.codex != nil {
			for id := range d.codex.Live() {
				m[id] = true
			}
		}
		return m
	})
	// Control mode annotates a row as structured when it has a live supervisor, so
	// a consumer gates its affordances on the actual control path (§2.2).
	ws.SetControlMode(func(id string) string {
		if d.codex != nil {
			if _, ok := d.codex.Get(id); ok {
				return harnessproto.ControlModeStructured
			}
		}
		return ""
	})
	ws.SetPRs(d.sessionPRs)
	ws.SetGoalStatus(d.coordinatorGoalStatus)
	ws.SetModelObserved(func(id, kind, model string) {
		if kind == "codex" && d.codex != nil {
			d.codex.SetModel(id, model)
		}
	})
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
	s, ok, err := lookupSession(k.AgentID)
	if err != nil || !ok {
		return engine.ActivityUnknown
	}
	return agent.HarnessFor(s.Agent).Activity(s)
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
	if d.configErr != nil {
		return d.configErr
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	ctx = runCtx
	// The session mailboxes outlive runCtx: teardown keeps serving their
	// reports through the engine's wait-for-idle, then cancels this itself.
	rpcCtx, cancelRPC := context.WithCancel(context.WithoutCancel(runCtx))
	var rpcWG sync.WaitGroup
	var runWG sync.WaitGroup
	startRunWorker := func(work func()) {
		runWG.Add(1)
		go func() {
			defer runWG.Done()
			work()
		}()
	}
	var (
		lock                *os.File
		ln                  net.Listener
		cleanupSocket       func()
		authorityOwned      bool
		sessionRPCOwned     bool
		codexOwned          bool
		engineOwned         bool
		persistLiveOnReturn bool
	)
	// One teardown path owns both normal cancellation and every partial startup
	// failure after the daemon lifetime context exists. Do not replace this with
	// independent defers: their LIFO ordering previously stopped engines before
	// RPC completion and background owners had drained.
	defer func() {
		d.status.Begin(daemonstatus.OpStopping, daemonstatus.PhaseDraining)
		d.stopDeferredAdmission()
		if sessionRPCOwned {
			d.sessionRPC.reportsOnly.Store(true)
		}
		cancelRun()
		// Retire structured-control transports before joining anything that may be
		// blocked in an admitted JSON-RPC write. This is intentionally earlier than
		// Shutdown below: process/engine teardown and authority release still happen
		// only after every admitted caller and serving connection has returned.
		if codexOwned {
			d.codex.InterruptTransports()
		}
		if ln != nil {
			_ = ln.Close()
		}
		d.drainServingConnections()
		runWG.Wait()
		// Deferred effects may enter the same admission locks used by completion
		// cleanup. Join them before sessionRuntime.close takes dispatch ownership
		// and waits for completion owners, avoiding an inverted shutdown wait.
		d.deferredWG.Wait()
		// A Claude agent's turn ending reaches the daemon only as a hook report
		// through its session mailbox, so wait for mid-turn agents while the
		// mailboxes still serve reports (and nothing else). Closing them first
		// would leave every busy agent busy until the grace ran out. Reports stop
		// before the restart journal records which agents were mid-turn and
		// before termination, whose exit hooks must not rewrite that state.
		if engineOwned {
			if w, ok := d.engine.(idleWaiter); ok {
				d.status.Phase(daemonstatus.PhaseAgents)
				d.observeEngineShutdown()
				w.WaitIdle()
			}
		}
		cancelRPC()
		rpcWG.Wait()
		if sessionRPCOwned {
			d.sessionRPC.close()
		}
		if persistLiveOnReturn {
			d.persistLiveAgents()
		}
		if codexOwned {
			d.status.Phase(daemonstatus.PhaseCodex)
			d.codex.Shutdown()
			d.closeCentralCodex()
		}
		if engineOwned {
			d.status.Phase(daemonstatus.PhaseAgents)
			d.observeEngineShutdown()
			d.engine.Shutdown()
		}
		d.status.Phase(daemonstatus.PhaseReleasing)
		if cleanupSocket != nil {
			cleanupSocket()
		}
		if authorityOwned {
			_ = d.authority.Close()
		}
		// The status file belongs to the lock holder; drop it while still
		// holding the lock so it never outlives this daemon's ownership.
		d.status.Clear()
		releaseSingletonLock(lock)
	}()
	startRunWorker(func() {
		select {
		case <-d.shutdown:
			cancelRun()
		case <-runCtx.Done():
		}
	})
	log.Printf("codex control: %s (source=%s, config=%s, persisted=%q, override_set=%t, override=%q)",
		d.codexControl.Effective, d.codexControl.Source, d.codexControl.ConfigPath,
		d.codexControl.Persisted, d.codexControl.OverrideSet, d.codexControl.Override)
	if d.codexControl.Warning != "" {
		log.Print(d.codexControl.Warning)
	}
	if err := os.MkdirAll(core.StateDir(), 0o755); err != nil {
		return err
	}
	var err error
	lock, err = acquireSingletonLock()
	if err != nil {
		return err
	}
	d.status = daemonstatus.NewReporter(daemonstatus.Path())
	d.status.Begin(daemonstatus.OpStarting, daemonstatus.PhaseReconciling)
	// Acquiring singleton ownership commits this Run to owning the injected or
	// default engine through every later startup failure. Persistence begins only
	// after the listener and restore state are ready, but engine teardown does not
	// depend on reaching that point.
	engineOwned = d.engine != nil
	if d.authority == nil {
		d.authority, err = access.OpenDefault()
		if err != nil {
			return fmt.Errorf("open daemon authority: %w", err)
		}
	}
	authorityOwned = true
	if err := d.ensureHostCredential(ctx, time.Now()); err != nil {
		return fmt.Errorf("provision host credential: %w", err)
	}
	d.sessionRPC = newSessionRuntime(d)
	sessionRPCOwned = true
	d.sessionRPC.progress = d.status.Count
	err = d.sessionRPC.start(ctx)
	d.sessionRPC.progress = nil
	if err != nil {
		return fmt.Errorf("start session RPC: %w", err)
	}
	rpcWG.Add(1)
	go func() {
		defer rpcWG.Done()
		d.sessionRPC.run(rpcCtx)
	}()
	sock := core.SocketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	d.status.Phase(daemonstatus.PhaseListening)
	ln, cleanupSocket, err = d.listen(sock, d.authority.BootID())
	if err != nil {
		return err
	}
	// Ready: a client's probe now succeeds, so there is no progress to report
	// until shutdown begins.
	d.status.Clear()

	// The structured-control supervisor manager is bound to the daemon's context:
	// its App Servers live and die with the daemon, not with any pane or client.
	// It stays inert unless the captured startup selection enables App Server control.
	d.codex = codexapp.NewManager(ctx, os.Getenv("AMUX_CODEX_BIN"))
	d.codex.SetPreStart(importBeforeLaunch)
	codexOwned = true
	d.startCentralCodex(ctx)
	// Carry PTY-era Codex history into the structured logs that are now every
	// Codex session's canonical source, without delaying startup: each launch
	// also imports first (the manager's pre-start hook), and the per-session
	// quiescent-log gate serializes the two.
	startRunWorker(d.importCodexHistories)
	// Budgets refresh independently of clients and sessions of either harness.
	startRunWorker(func() { d.accountLimitsLoop(ctx) })
	// The engine's agents live in this process; stop them cleanly on shutdown.
	// (Agents survive a UI restart — the daemon stays up — but not a daemon
	// restart, e.g. `amux daemon restart`; out-of-process hosting would lift that.)
	// Persist the live set before killing them, so the next startup relaunches it.
	// Read the previously-live set BEFORE any poll persists over the file, then
	// relaunch it once sessions/specs are resolvable (after the first poll).
	d.pendingRestore = d.readLiveAgents()
	persistLiveOnReturn = d.engine != nil
	startRunWorker(func() {
		select {
		case <-ctx.Done():
		case <-d.firstPoll:
			d.restoreLiveAgents(ctx)
		}
	})

	startRunWorker(func() { d.pollLoop(ctx) })
	// Member PRs are polled on their own (much slower) clock.
	startRunWorker(func() { d.prWatchLoop(ctx) })
	// Per-session CPU/memory, sampled off the RPC path.
	d.startResourceSampler(ctx, startRunWorker)

	// Close the listener when ctx ends so Accept returns.
	startRunWorker(func() { <-ctx.Done(); _ = ln.Close() })

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		d.startServingConnection(ctx, conn)
	}
}

// shutdownObservable is the engine hook that reports per-instance progress
// through a graceful Shutdown (the local engine implements it).
type shutdownObservable interface {
	SetShutdownObserver(func([]local.ShutdownProgress))
}

// idleWaiter is the engine hook that runs a graceful Shutdown's wait-for-idle
// on its own, so the daemon can keep serving agents' activity reports during
// it (the local engine implements it).
type idleWaiter interface {
	WaitIdle()
}

// observeEngineShutdown routes the engine's per-instance shutdown progress to
// the status file, labelled with each session's title as last polled.
func (d *Daemon) observeEngineShutdown() {
	obs, ok := d.engine.(shutdownObservable)
	if !ok || d.status == nil {
		return
	}
	titles := map[string]string{}
	d.mu.RLock()
	for _, s := range d.sessions {
		titles[s.ID] = s.Title
	}
	d.mu.RUnlock()
	obs.SetShutdownObserver(func(progress []local.ShutdownProgress) {
		agents := make([]daemonstatus.Agent, 0, len(progress))
		for _, p := range progress {
			a := daemonstatus.Agent{ID: p.Key.AgentID, Title: titles[p.Key.AgentID], Tab: p.Key.Tab, State: p.State}
			if !p.Until.IsZero() {
				a.Until = p.Until.UnixMilli()
			}
			agents = append(agents, a)
		}
		d.status.Agents(agents)
	})
}

// startDeferredWork registers action-owned asynchronous work before launching
// it. Registration and shutdown admission share deferredMu, preventing Add from
// racing the shutdown Wait. The work receives the already-derived daemon
// lifetime context from its caller; cancellation precedes the shutdown wait.
func (d *Daemon) startDeferredWork(work func()) bool {
	d.deferredMu.Lock()
	if d.deferredDraining {
		d.deferredMu.Unlock()
		return false
	}
	d.deferredWG.Add(1)
	d.deferredMu.Unlock()
	go func() {
		defer d.deferredWG.Done()
		work()
	}()
	return true
}

// startDeferredContext is the action-owned form of startDeferredWork. Session
// mailbox callbacks carry a short callback deadline, but an accepted cold start
// or prompt is explicitly asynchronous and must outlive the response that ACKs
// it. deferredContext preserves the request's values/authorization guard while
// rebinding cancellation to the serving lifetime; shutdown still joins it via
// deferredWG before engines, supervisors, or authority are torn down.
func (d *Daemon) startDeferredContext(ctx context.Context, work func(context.Context)) bool {
	workCtx, release := deferredContext(ctx)
	if !d.startDeferredWork(func() {
		defer release()
		work(workCtx)
	}) {
		release()
		return false
	}
	return true
}

func (d *Daemon) stopDeferredAdmission() {
	d.deferredMu.Lock()
	d.deferredDraining = true
	d.deferredMu.Unlock()
}

// startServingConnection registers an accepted transport before its handler is
// launched. Once draining starts, no new handler can race WaitGroup.Wait.
func (d *Daemon) startServingConnection(ctx context.Context, conn net.Conn) {
	d.startServingHandler(ctx, conn, d.serve)
}

func (d *Daemon) startServingHandler(ctx context.Context, conn net.Conn, serve func(context.Context, net.Conn)) {
	d.servingMu.Lock()
	if d.servingDraining {
		d.servingMu.Unlock()
		_ = conn.Close()
		return
	}
	id := d.servingNext
	d.servingNext++
	d.serving[id] = conn
	d.servingWG.Add(1)
	d.servingMu.Unlock()

	go func() {
		defer func() {
			d.servingMu.Lock()
			delete(d.serving, id)
			d.servingMu.Unlock()
			d.servingWG.Done()
		}()
		serve(ctx, conn)
	}()
}

// drainServingConnections closes all accepted transports before waiting for
// their handlers. Closing the raw connection unblocks both TLS handshakes and
// writes; the authority remains owned until every handler has returned.
func (d *Daemon) drainServingConnections() {
	d.servingMu.Lock()
	d.servingDraining = true
	connections := make([]net.Conn, 0, len(d.serving))
	for _, conn := range d.serving {
		connections = append(connections, conn)
	}
	d.servingMu.Unlock()

	for _, conn := range connections {
		_ = conn.Close()
	}
	d.servingWG.Wait()
}

func (d *Daemon) requestShutdown() {
	d.shutdownOnce.Do(func() { close(d.shutdown) })
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
	d.resumeWithSharedAuth(ctx)

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
	if state, err := accounts.Read(); err == nil {
		for i := range sess {
			if b, ok := state.Bindings[sess[i].ID]; ok {
				sess[i].AccountBinding = &harnessproto.AccountBinding{ProfileID: b.ProfileID, Source: b.Source}
			}
		}
	}
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
	d.serveAuthenticated(ctx, conn, false)
}

// consoleOnly selects a distinct listener; it is never client-controlled.
func (d *Daemon) serveAuthenticated(ctx context.Context, conn net.Conn, consoleOnly bool) {
	defer conn.Close()
	if d.authority == nil {
		return
	}
	tlsConfig, err := d.authority.ServerTLSConfig()
	if err != nil {
		return
	}
	secure := tls.Server(conn, tlsConfig)
	_ = secure.SetDeadline(time.Now().Add(authDeadline))
	if err := secure.HandshakeContext(ctx); err != nil {
		return
	}
	kind := access.SubjectHost
	if consoleOnly {
		kind = access.SubjectSession
	}
	principal, reader, err := d.authenticateSubject(ctx, secure, kind)
	if err != nil {
		return
	}

	valid := func(checkCtx context.Context) error {
		if consoleOnly {
			return d.authorizeConsoleAccounts(checkCtx, principal)
		}
		return d.authority.Valid(checkCtx, principal)
	}

	invalidated, cancelWatch, err := d.authority.WatchInvalidation(principal)
	if err != nil {
		return
	}
	defer cancelWatch()
	connectionCtx, cancelConnection := context.WithCancel(ctx)
	defer cancelConnection()
	cl := newAuthenticatedConnState(secure, func() bool { return connectionCtx.Err() == nil && valid(ctx) == nil })
	watchDone := make(chan struct{})
	var snapshotDone chan struct{}
	go func() {
		defer close(watchDone)
		defer cancelConnection()
		defer conn.Close()
		select {
		case <-invalidated:
			// Abort a blocked TLS write as well as queued frames. A write that has
			// not completed when revocation commits must not drain afterward.
		case <-cl.done:
		case <-connectionCtx.Done():
		}
	}()
	defer func() {
		cancelConnection()
		_ = conn.Close()
		d.disconnectAccounts(cl.accountID, true)
		cl.shutdown()
		if snapshotDone != nil {
			<-snapshotDone
		}
		<-watchDone
	}()

	if !consoleOnly {
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
		if valid(ctx) != nil {
			return
		}
		cl.send(d.snapshot())

		// Push subsequent snapshots.
		snapshotDone = make(chan struct{})
		go func() {
			defer close(snapshotDone)
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
					if valid(ctx) != nil {
						cl.stop()
						return
					}
					cl.send(snap)
				}
			}
		}()

		// Deferred work carries the same generation guard and rechecks it at the
		// actual execution boundary, not merely when this loop accepts the frame.
	}
	clientCtx := withAccessGuard(ctx, func(checkCtx context.Context) error {
		return valid(checkCtx)
	})

	// Account attempts belong to the live socket as well as its principal.
	// Ordinary acknowledged deferred actions retain their existing daemon
	// lifetime; only account ceremonies use this connection-bound guard.
	accountCtx := withAccessGuard(connectionCtx, func(checkCtx context.Context) error {
		if err := connectionCtx.Err(); err != nil {
			return err
		}
		return valid(checkCtx)
	})
	actions, readDone := readHostActions(reader, cancelConnection)
	defer func() { cancelConnection(); _ = conn.Close(); <-readDone }()
	for {
		var a core.Action
		select {
		case <-connectionCtx.Done():
			return
		case next, ok := <-actions:
			if !ok {
				return
			}
			a = next
		}
		if connectionCtx.Err() != nil || valid(ctx) != nil {
			return
		}
		if consoleOnly {
			if a.Action != core.ActionQuery || (a.Query != core.QueryAccounts && a.Query != core.QueryAccountManage && a.Query != core.QueryAccountCeremony) {
				return
			}
			d.query(accountCtx, cl, a)
			continue
		}
		switch a.Action {
		case core.ActionPaneOpen:
			d.paneOpen(clientCtx, cl, a)
		case core.ActionPaneInput:
			cl.paneInput(a.PaneID, a.Data)
		case core.ActionPaneResize:
			cl.paneResize(a.PaneID, a.Cols, a.Rows)
		case core.ActionPaneClose:
			cl.paneClose(a.PaneID)
		case core.ActionQuery:
			d.query(accountCtx, cl, a)
		default:
			actionCtx := clientCtx
			if a.Action == core.ActionAccountManage {
				actionCtx = accountCtx
			}
			res := d.handle(actionCtx, a)
			if a.Action != "" {
				cl.send(res)
			}
		}
	}
}

// readHostActions keeps observing EOF while dispatch waits for effect admission.
// Frames remain ordered. Bounds cover both frame bytes and outstanding actions;
// overload retires the connection rather than hiding EOF behind a blocked queue
// or accepting an unbounded amount of work. Client encoders use newline JSON.
const hostActionQueue = 32
const hostActionMaxBytes = clientFrameLimit

func readHostActions(reader *bufio.Reader, retire context.CancelFunc) (<-chan core.Action, <-chan struct{}) {
	actions := make(chan core.Action, hostActionQueue)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(actions)
		defer retire()
		for {
			line, err := readBoundedLine(reader, hostActionMaxBytes)
			if err != nil || len(line) > hostActionMaxBytes {
				return
			}
			var action core.Action
			if json.Unmarshal(line, &action) != nil {
				return
			}
			select {
			case actions <- action:
			default:
				return
			}
		}
	}()
	return actions, done
}

type accessGuardContextKey struct{}
type effectAdmissionContextKey struct{}
type deferredLifetimeContextKey struct{}

func withAccessGuard(ctx context.Context, valid func(context.Context) error) context.Context {
	return context.WithValue(ctx, accessGuardContextKey{}, valid)
}

// withDeferredLifetime records the owner that remains valid after a bounded
// transport callback returns. Values added below this marker (notably the
// current-principal authorization guard) are retained by deferredContext, while
// cancellation is rebound from the callback-local deadline to this lifetime.
func withDeferredLifetime(ctx, lifetime context.Context) context.Context {
	return context.WithValue(ctx, deferredLifetimeContextKey{}, lifetime)
}

// deferredContext detaches only a session-RPC callback's local timeout. The
// resulting context is still cancelled by the daemon/session-RPC serving
// lifetime and still carries the authorization guard that is re-run under final
// effect admission. Ordinary host and internal callers have no lifetime marker
// and keep their original context unchanged.
func deferredContext(ctx context.Context) (context.Context, func()) {
	lifetime, _ := ctx.Value(deferredLifetimeContextKey{}).(context.Context)
	if lifetime == nil {
		return ctx, func() {}
	}
	deferred, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopLifetime := context.AfterFunc(lifetime, cancel)
	// AfterFunc schedules asynchronously when lifetime is already done. Close
	// that admission window synchronously; cancellation racing this check is
	// still covered by the registered callback.
	if lifetime.Err() != nil {
		cancel()
	}
	return deferred, func() {
		stopLifetime()
		cancel()
	}
}

func withEffectAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, effectAdmissionContextKey{}, true)
}

func effectAdmissionHeld(ctx context.Context) bool {
	held, _ := ctx.Value(effectAdmissionContextKey{}).(bool)
	return held
}

// revalidateDeferred is a no-op for internal/test callers without a streaming
// principal. Work accepted from an authenticated connection carries a guard.
func revalidateDeferred(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	valid, _ := ctx.Value(accessGuardContextKey{}).(func(context.Context) error)
	if valid == nil {
		return nil
	}
	return valid(ctx)
}

const authDeadline = 5 * time.Second

// authenticateHost completes authentication before the caller creates a write
// pump, subscription, or pane route. The host listener accepts only explicitly
// issued host keys; session and provider keys cannot unlock streaming by selecting
// a different socket path.
func (d *Daemon) authenticateHost(ctx context.Context, conn net.Conn) (access.Principal, *bufio.Reader, error) {
	return d.authenticateSubject(ctx, conn, access.SubjectHost)
}

func (d *Daemon) authenticateSubject(ctx context.Context, conn net.Conn, kind access.SubjectKind) (access.Principal, *bufio.Reader, error) {
	if d.authority == nil {
		return access.Principal{}, nil, fmt.Errorf("daemon authority unavailable")
	}
	_ = conn.SetDeadline(time.Now().Add(authDeadline))
	defer conn.SetDeadline(time.Time{})
	challenge, err := d.authority.NewChallenge()
	if err != nil {
		return access.Principal{}, nil, err
	}
	if err := json.NewEncoder(conn).Encode(challenge); err != nil {
		return access.Principal{}, nil, err
	}
	// ReadSlice on a fixed-capacity reader makes the pre-auth proof an actual
	// byte bound. ReadBytes would allocate without limit until a newline arrived.
	reader := bufio.NewReaderSize(conn, access.MaxBodyBytes+1)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return access.Principal{}, nil, err
	}
	if len(line) > access.MaxBodyBytes {
		return access.Principal{}, nil, access.ErrInvalidCredential
	}
	var proof access.SocketProof
	if err := json.Unmarshal(line, &proof); err != nil {
		_ = json.NewEncoder(conn).Encode(access.SocketWelcome{Type: access.FrameWelcome, Error: "authentication failed"})
		return access.Principal{}, nil, access.ErrInvalidCredential
	}
	principal, err := d.authority.VerifyProof(ctx, challenge, proof, kind)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(access.SocketWelcome{Type: access.FrameWelcome, Error: "authentication failed"})
		return access.Principal{}, nil, err
	}
	if kind == access.SubjectSession {
		if err := d.authorizeConsoleAccounts(ctx, principal); err != nil {
			return access.Principal{}, nil, err
		}
	}
	if err := json.NewEncoder(conn).Encode(access.SocketWelcome{Type: access.FrameWelcome, OK: true}); err != nil {
		return access.Principal{}, nil, err
	}
	return principal, reader, nil
}

// listenOwnedUnix binds a boot-unique socket, then publishes it through the
// stable socket pathname as a relative symlink. The singleton lock must already
// be held. Replacing an old managed symlink never unlinks its socket inode; an
// unmanaged legacy socket/file is refused rather than guessed stale. Cleanup
// removes only this boot's unique socket and deliberately leaves a dangling
// managed symlink for the next lock owner to replace.
func listenOwnedUnix(stable, bootID string) (net.Listener, func(), error) {
	if len(bootID) < 16 {
		return nil, nil, fmt.Errorf("invalid daemon boot identity")
	}
	unique := stable + "." + bootID[:16]
	ln, err := net.Listen("unix", unique)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = ln.Close()
		_ = os.Remove(unique)
	}
	if info, err := os.Lstat(stable); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			cleanup()
			return nil, nil, fmt.Errorf("refusing to replace unmanaged daemon endpoint %s", stable)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		cleanup()
		return nil, nil, err
	}
	pending := stable + ".link." + bootID[:16]
	_ = os.Remove(pending) // boot-unique path owned by this lock holder
	if err := os.Symlink(filepath.Base(unique), pending); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := os.Rename(pending, stable); err != nil {
		_ = os.Remove(pending)
		cleanup()
		return nil, nil, err
	}
	return ln, cleanup, nil
}

func acquireSingletonLock() (*os.File, error) {
	f, err := os.OpenFile(core.DaemonLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("daemon already running (singleton lock %s): %w", core.DaemonLockPath(), err)
	}
	return f, nil
}

func releaseSingletonLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
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
	spec, err := d.launchSpec(ctx, a.ID)
	if err != nil {
		paneExit(err.Error())
		return
	}
	dir, env, argv, err := d.resolve(spec, a.Tab)
	// Opening the AGENT tab of a structured session must NOT start a standalone
	// Codex: it attaches the native TUI to the supervised server/thread via
	// `codex --remote <endpoint> resume <thread>` (AGE-181). Ensure the supervisor
	// first so the server is up and the thread pinned, then resolve the attach argv.
	if a.Tab == panespec.TabAgent {
		if d.structuredControl(spec.Session) {
			sup, e := d.ensureSupervisorSpec(ctx, spec)
			if e != nil {
				paneExit(e.Error())
				return
			}
			// Attach at the LIVE server's endpoint and thread — a single source of
			// truth, so the native TUI and the web bridge share one server/thread.
			id := sup.Identity()
			dir, env, argv, err = panespec.AttachCommand(spec, id.Endpoint, id.ThreadID)
		}
	}
	if err != nil {
		paneExit(err.Error())
		return
	}
	if err := revalidateDeferred(ctx); err != nil {
		retireUnstarted(a.ID, argv)
		paneExit(err.Error())
		return
	}
	engineSpec := engine.Spec{
		Key: engine.Key{AgentID: a.ID, Tab: a.Tab},
		Dir: dir, Env: env, ModelAccess: launchenv.ForRuntime(spec.Session.Agent),
		Argv: argv, Cols: a.Cols, Rows: a.Rows,
	}
	var inst engine.Instance
	if a.Tab == panespec.TabAgent && !d.structuredControl(spec.Session) {
		published, _, publishErr := d.publishPermissionRuntime(a.ID, func() (any, error) {
			return d.ensurePane(ctx, engineSpec)
		})
		err = publishErr
		if err == nil {
			inst, _ = published.(engine.Instance)
		}
	} else {
		inst, err = d.ensurePane(ctx, engineSpec)
	}
	if err != nil {
		paneExit(err.Error())
		return
	}
	if inst == nil {
		paneExit("engine returned no runtime")
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
//
// A structured-control Codex session (AGE-181) is started as an App Server
// supervisor instead of a PTY pane: the supervisor is the runtime, owned by the
// daemon and independent of any pane. Everything else is a PTY pane exactly as
// before.
func (d *Daemon) startEngineFor(ctx context.Context, id string) error {
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	if err := revalidateDeferred(ctx); err != nil {
		return err
	}
	ids, err := d.agentsUnder(id)
	if err != nil {
		return err
	}
	return d.startAgents(ctx, id, ids)
}

// startAgents starts exactly ids, the agents resolved for id.
func (d *Daemon) startAgents(ctx context.Context, id string, ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("no agent to start for %q", id)
	}
	var firstErr error
	for _, aid := range ids {
		if err := revalidateDeferred(ctx); err != nil {
			return err
		}
		err := d.startAgent(ctx, aid)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// startAgent starts one session's process — the TabAgent pane, or the App Server
// supervisor for a structured-control Codex session — without attaching a UI.
// Ensure is idempotent, so a running session is a no-op. This is the single
// session-start primitive: startEngineFor fans a root out to its members through
// it, and a `prompt` to a stopped session starts exactly that session with it.
//
// A start resumes an eligible goal (restartIntent): `amux do start`, the daemon's
// own restore and an agent restart all count as the runtime coming back. The
// optional restart record carries the journal's evidence for that session.
func (d *Daemon) startAgent(ctx context.Context, aid string, restart ...restartRecord) error {
	intent := startIntent{resumeGoal: true}
	if len(restart) > 0 {
		intent.record = &restart[0]
	}
	return d.startAgentWith(ctx, aid, intent)
}

// startAgentWith is startAgent with the launch's restart intent spelled out.
func (d *Daemon) startAgentWith(ctx context.Context, aid string, intent startIntent) error {
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	spec, err := d.launchSpec(ctx, aid)
	if err != nil {
		return err
	}
	work := d.restartIntent(intent, &spec)
	if d.structuredControl(spec.Session) {
		_, err := d.ensureSupervisorSpec(ctx, spec, work)
		return err
	}
	dir, env, argv, err := d.resolve(spec, panespec.TabAgent)
	if err != nil {
		return err
	}
	if err := revalidateDeferred(ctx); err != nil {
		retireUnstarted(aid, argv)
		return err
	}
	published, _, err := d.publishPermissionRuntime(aid, func() (any, error) {
		return d.ensurePane(ctx, engine.Spec{
			Key: engine.Key{AgentID: aid, Tab: panespec.TabAgent},
			Dir: dir, Env: env, ModelAccess: launchenv.ForRuntime(spec.Session.Agent), Argv: argv,
		})
	})
	if err != nil {
		retireUnstarted(aid, argv)
		return err
	}
	if _, ok := published.(engine.Instance); !ok {
		return fmt.Errorf("engine returned no runtime")
	}
	return nil
}

// ensureSupervisor starts (or returns) the App Server supervisor for a structured
// session, launched under the amux sandbox wrapper (panespec.AppServerCommand) so
// it inherits the session's mount/config/identity scope — not a bare exec. The
// endpoint is the per-session WebSocket socket in the private scope.
func (d *Daemon) ensureSupervisor(ctx context.Context, agentID string) (*codexapp.Supervisor, error) {
	// A live supervisor already has its endpoint; reuse it so we don't rebuild argv.
	if sup, ok := d.codex.Get(agentID); ok {
		return sup, nil
	}
	spec, err := d.launchSpec(ctx, agentID)
	if err != nil {
		return nil, err
	}
	return d.ensureSupervisorSpec(ctx, spec)
}

func (d *Daemon) ensureSupervisorSpec(ctx context.Context, spec panespec.LaunchSpec, restart ...*codexapp.RestartWork) (*codexapp.Supervisor, error) {
	agentID := spec.Session.ID
	if sup, ok := d.codex.Get(agentID); ok {
		return sup, nil
	}

	// AppServerCommand resolves the sandbox-wrapped launch AND the per-session unix
	// endpoint (in its separately mounted socket directory) — one source for the endpoint
	// baked into argv, dialed by amux, and persisted for a native attach.
	dir, env, argv, endpoint, err := panespec.AppServerCommand(spec)
	if err != nil {
		return nil, err
	}
	if err := revalidateDeferred(ctx); err != nil {
		return nil, err
	}
	sess := spec.Session
	var work *codexapp.RestartWork
	if len(restart) > 0 {
		work = restart[0]
	}
	published, _, err := d.publishPermissionRuntime(agentID, func() (any, error) {
		return d.codex.Ensure(ctx, agentID, dir, env, argv, endpoint, sess.Model, sess.Prompt, sess.ClaudeID, codexapp.LaunchOptions{Sandbox: panespec.CodexSandboxForLaunch(), RestartWork: work, Goals: agent.NativeGoals(sess)})
	})
	if err != nil {
		return nil, err
	}
	sup, ok := published.(*codexapp.Supervisor)
	if !ok {
		return nil, fmt.Errorf("App Server returned no runtime")
	}
	return sup, nil
}

// structuredControl applies the startup selection to Codex sessions only.
// Configuration is never re-read while routing existing or new sessions. A
// goal session (a Codex coordinator) is structured whatever the machine-wide
// selection says: its native goal lives in the App Server, so that is the only
// runtime that can run it. Ordinary Codex agents keep following codex.control.
func (d *Daemon) structuredControl(s store.Session) bool {
	if d.codex == nil || agent.Canonical(s.Agent) != harnessproto.RuntimeCodex {
		return false
	}
	return d.codexControl.Effective == amuxcfg.AppServer || agent.NativeGoals(s)
}

// persistLiveAgents writes the current set of live engine keys to disk so a
// daemon restart can relaunch them without a UI trigger. It skips the write when
// the set is unchanged from the last one, so the per-poll call is cheap. Failures
// are logged, never fatal — persistence is best-effort.
func (d *Daemon) persistLiveAgents() {
	if d.liveAgentsPath == "" {
		return
	}
	var keys []engine.Key
	if d.engine != nil {
		keys = d.engine.Live()
	}
	if d.codex != nil {
		for id := range d.codex.RestartSnapshot() {
			key := engine.Key{AgentID: id, Tab: panespec.TabAgent}
			found := false
			for _, k := range keys {
				if k == key {
					found = true
					break
				}
			}
			if !found {
				keys = append(keys, key)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].AgentID != keys[j].AgentID {
			return keys[i].AgentID < keys[j].AgentID
		}
		return keys[i].Tab < keys[j].Tab
	})
	records := make([]restartRecord, 0, len(keys))
	for _, key := range keys {
		records = append(records, d.captureRestart(key))
	}
	buf, err := json.Marshal(records)
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
func (d *Daemon) readLiveAgents() []restartRecord {
	if d.liveAgentsPath == "" {
		return nil
	}
	buf, err := os.ReadFile(d.liveAgentsPath)
	if err != nil {
		return nil
	}
	var keys []restartRecord
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
		if err := d.startAgent(ctx, k.AgentID, k); err != nil {
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

// killEngineFor stops all tabs of the target and its stored direct members.
// Membership must come from the store: a stale rail can omit newly added members
// or still list a member that has moved into a foreign workgroup.
func (d *Daemon) killEngineFor(id string) {
	if d.engine == nil && d.codex == nil {
		return
	}
	d.authMu.Lock()
	defer d.authMu.Unlock()
	ids := map[string]bool{id: true}
	if db, err := store.OpenReadOnly(); err == nil {
		children, err := db.Children(id)
		db.Close()
		if err != nil {
			log.Printf("session %s member shutdown lookup: %v", id, err)
		} else {
			for _, child := range children {
				ids[child.ID] = true
			}
		}
	} else {
		// The durable parent archive flag also drives member reconciliation, including
		// recovery after this lookup fails. Never fall back to stale ownership.
		log.Printf("session %s shutdown store: %v", id, err)
	}
	for aid := range ids {
		d.killRuntimeLocked(aid)
	}
}

// ensurePane is the engine's Ensure for every daemon pane start. A resolved
// central Claude argv carries a freshly allocated account launch. When Ensure
// reuses the instance that was already running, or fails to spawn, no process
// received that launch, so it is retired; a pane that exited between the
// caller's resolve and Ensure is respawned and keeps its own fresh launch.
// retireUnstarted withdraws the account launch a resolved argv carries when
// the caller abandons it before ensurePane (a revalidation or publication
// failure, a skipped restart). It is a no-op once that launch was delivered.
func retireUnstarted(id string, argv []string) {
	if launch, ok := panespec.AccountLaunchOf(argv); ok {
		if err := accounts.RetireLaunch(id, launch); err != nil {
			log.Printf("amux: retire unused account launch for %s: %v", id, err)
		}
	}
}

func (d *Daemon) ensurePane(ctx context.Context, spec engine.Spec) (engine.Instance, error) {
	before, alive := d.engine.Lookup(spec.Key)
	alive = alive && before.Alive()
	in, err := d.engine.Ensure(ctx, spec)
	if launch, ok := panespec.AccountLaunchOf(spec.Argv); ok && (err != nil || (alive && in == before)) {
		if rerr := accounts.RetireLaunch(spec.Key.AgentID, launch); rerr != nil {
			log.Printf("amux: retire unused account launch for %s: %v", spec.Key.AgentID, rerr)
		}
	}
	return in, err
}

func (d *Daemon) killRuntimeFor(id string) {
	if d.engine == nil && d.codex == nil {
		return
	}
	d.authMu.Lock()
	defer d.authMu.Unlock()
	d.killRuntimeLocked(id)
}

// killRuntimeToken stops only the exact incarnation captured by a lifecycle
// owner. A stale completion must never kill a replacement published under the
// same session id.
func (d *Daemon) killRuntimeToken(id string, token permissionRuntimeToken) bool {
	if d.engine == nil && d.codex == nil {
		return false
	}
	d.authMu.Lock()
	defer d.authMu.Unlock()
	return d.permissions.retireTokenAnd(id, token, func() {
		delete(d.authPending, engine.Key{AgentID: id, Tab: panespec.TabAgent})
		if d.engine != nil {
			for tab := 0; tab < 3; tab++ {
				d.engine.Kill(engine.Key{AgentID: id, Tab: tab})
			}
		}
		d.closeCodex(id)
	})
}

func (d *Daemon) killRuntimeLocked(id string) {
	d.permissions.retireAnd(id, func() {
		delete(d.authPending, engine.Key{AgentID: id, Tab: panespec.TabAgent})
		if d.engine != nil {
			for tab := 0; tab < 3; tab++ { // agent | editor | terminal
				d.engine.Kill(engine.Key{AgentID: id, Tab: tab})
			}
		}
		// A structured session runs under the supervisor, not a pane. Close keeps
		// its persisted identity so recreation/unarchive resumes the same thread.
		d.closeCodex(id)
	})
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
