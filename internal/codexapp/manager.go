package codexapp

import (
	"amux/internal/accounts"

	"context"
	"errors"
	"sync"

	"amux/internal/launchenv"
)

// manager.go owns the set of live supervisors for the daemon (AGE-181). It is the
// daemon's single handle onto structured control: the daemon asks it to Ensure a
// supervisor when a structured session starts, Get one to route a steer verb or
// resolve the event record, and Close one when a session stops. Supervisors are
// keyed by amux session id and live for the daemon's context — never a pane — so
// this is where "amux owns the App Server lifetime" is actually enforced.
//
// The Manager holds no protocol knowledge; it wires Config (socket path, event
// log, resume thread) from the persisted identity and delegates everything else
// to the Supervisor.
type Manager struct {
	ctx context.Context // daemon admission lifetime; cancellation starts transport drain
	// supervisorCtx is cancelled only by Shutdown. Separating it from ctx lets a
	// daemon cancel interrupt control I/O before joins without concurrently killing
	// the process; full process teardown remains ordered after the joins.
	supervisorCtx     context.Context
	cancelSupervisors context.CancelFunc
	stopContextWatch  func() bool
	bin               string // codex binary override (AMUX_CODEX_BIN), "" ⇒ "codex"
	chatgptAuth       TokenProvider

	mu       sync.Mutex
	sup      map[string]*Supervisor
	starting map[string]*Supervisor   // visible to shutdown before Start attaches its transport
	starts   map[string]chan struct{} // per-session creation gate (serialize Ensure before spawn)
	stopping bool                     // one-way: no supervisor may publish after drain begins
}

var errManagerStopping = errors.New("codexapp: manager stopping")

// NewManager builds a Manager bound to the daemon's context. bin overrides the
// codex binary (pass the resolved AMUX_CODEX_BIN or "").
func NewManager(ctx context.Context, bin string) *Manager {
	supervisorCtx, cancelSupervisors := context.WithCancel(context.WithoutCancel(ctx))
	m := &Manager{
		ctx:               ctx,
		supervisorCtx:     supervisorCtx,
		cancelSupervisors: cancelSupervisors,
		bin:               bin,
		sup:               map[string]*Supervisor{},
		starting:          map[string]*Supervisor{},
		starts:            map[string]chan struct{}{},
	}
	m.stopContextWatch = context.AfterFunc(ctx, m.InterruptTransports)
	return m
}

// startGate returns the per-session lifecycle semaphore, creating it once.
// Serializing Ensure/Close per session prevents two starts from competing and
// prevents an old supervisor's cleanup from unlinking a replacement listener,
// while still allowing a queued Ensure caller to honor context.
func (m *Manager) startGate(sessionID string) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	gate := m.starts[sessionID]
	if gate == nil {
		gate = make(chan struct{}, 1)
		gate <- struct{}{}
		m.starts[sessionID] = gate
	}
	return gate
}

// Get returns the live supervisor for a session id, or false. It is the daemon's
// "is this session structured right now?" test as well as the route target.
func (m *Manager) Get(sessionID string) (*Supervisor, bool) {
	m.mu.Lock()
	s, ok := m.sup[sessionID]
	m.mu.Unlock()
	if !ok || !s.live() {
		return nil, false
	}
	return s, true
}

// existingForEnsure performs the live lookup together with the one-way shutdown
// check. Ensure must not return even an already-live handle once transport drain
// has started, because its caller could otherwise begin new RPC work after the
// manager's interrupt snapshot.
func (m *Manager) existingForEnsure(ctx context.Context, sessionID string) (*Supervisor, bool, error) {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, false, errManagerStopping
	}
	s, ok := m.sup[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil, false, nil
	}
	if s.live() {
		return s, true, nil
	}
	if s.processRunning() {
		// A lost daemon-side WebSocket does not imply that the App Server, a
		// native client, or its current turn died. Rejoin the existing process;
		// never replace it merely because our control transport disappeared.
		reconnectCtx, cancelReconnect := context.WithCancel(m.supervisorCtx)
		stopAdmission := context.AfterFunc(ctx, cancelReconnect)
		err := s.reconnect(reconnectCtx)
		if !stopAdmission() || ctx.Err() != nil {
			cancelReconnect()
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if err != nil {
			cancelReconnect()
			return nil, false, err
		}
		return s, true, nil
	}

	// The owned process really exited. We hold the per-session start gate, so
	// teardown (including Unix-socket cleanup) completes before a successor can
	// bind the same endpoint.
	m.mu.Lock()
	if m.sup[sessionID] == s {
		delete(m.sup, sessionID)
	}
	m.mu.Unlock()
	_ = s.Close()
	return nil, false, nil
}

// SetChatGPTAuth selects the central ChatGPT token provider for supervisors
// started from now on (nil restores the legacy per-home credentials). Running
// supervisors keep the provider they started with.
func (m *Manager) SetChatGPTAuth(p TokenProvider) {
	m.mu.Lock()
	m.chatgptAuth = p
	m.mu.Unlock()
}

// SetModel updates a live supervisor's default for subsequent turns. It is a
// no-op for sessions not currently under structured control.
func (m *Manager) SetModel(sessionID, model string) {
	if s, ok := m.Get(sessionID); ok {
		s.SetModel(model)
	}
}

type LaunchOptions struct {
	// Sandbox is selected by the protected launcher. Empty retains the default.
	// This does not change approvals or their reviewer.
	Sandbox     string
	RestartWork *RestartWork
	Goals       bool // goal session: observed user tasks become the native goal
}

// Ensure returns the supervisor for a session, starting one if none is live. It
// fills the durable parts of the Config from the persisted identity (endpoint and,
// when known, the thread to resume) and the per-session event log, launches the
// App Server under the sandbox-wrapped argv bound to the daemon context, and
// persists the resulting identity so a later restart resumes the same thread.
// Idempotent: a second call returns the running supervisor unchanged.
//
// dir is the session's worktree; env are extra child-environment additions;
// wrappedArgv is the sandbox-wrapped `codex app-server --listen <endpoint>` the
// daemon resolved (nil only in tests / the direct-exec smoke path); endpoint is the
// listen/dial URL the daemon chose (loopback ws by default, unix optional) — the
// same value baked into wrappedArgv. model and initialPrompt are the selections
// stored on the amux session; the supervisor applies them to the structured
// thread and its first turn. legacyThreadID is the conversation id pinned by the
// older PTY control path; it is used only when no structured identity exists, so
// switching control modes adopts the existing conversation instead of starting
// over. Creation is serialized per session, so two callers never spawn competing
// servers. initialPrompt is submitted only when starting a fresh thread, never
// when reusing or resuming a supervisor.

func (m *Manager) Ensure(ctx context.Context, sessionID, dir string, env, wrappedArgv []string, endpoint, model, initialPrompt, legacyThreadID string, launch ...LaunchOptions) (*Supervisor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	// Serialize every start/reconnect/close transition for this session. Close
	// must finish killing the prior process and cleaning its Unix endpoint before
	// a replacement may bind that path. A request waiting behind another cold
	// start must not outlive its final-admission deadline.
	gate := m.startGate(sessionID)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.ctx.Done():
		return nil, m.ctx.Err()
	case <-gate:
	}
	defer func() { gate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.ctx.Err(); err != nil {
		return nil, err
	}
	if s, ok, err := m.existingForEnsure(ctx, sessionID); err != nil {
		return nil, err
	} else if ok {
		return s, nil
	}

	cfg := Config{
		SessionID:     sessionID,
		Bin:           m.bin,
		Dir:           dir,
		Env:           env,
		ModelAccess:   launchenv.ForRuntime("codex"),
		Model:         model,
		Endpoint:      endpoint,
		InitialPrompt: initialPrompt,
		EventLogPath:  EventLogPathFor(sessionID),
	}
	m.mu.Lock()
	if accounts.Mode(sessionID, "codex") == "central" {
		cfg.ChatGPTAuth = m.chatgptAuth
	}
	m.mu.Unlock()
	cfg.ResumeThreadID = resumeThreadFor(sessionID, legacyThreadID)
	// This is an actual process launch: it takes the newest central login, so
	// the resumed thread's previous account is history, not a constraint.
	cfg.PriorChatGPTAccounts = priorAccountsFor(sessionID, cfg.ResumeThreadID)
	if cfg.ChatGPTAuth != nil {
		launch, err := accounts.BeginLaunch(sessionID, "codex")
		if err != nil {
			return nil, err
		}
		cfg.AccountLaunch = launch
	}
	if len(launch) > 0 {
		cfg.Sandbox = launch[0].Sandbox
		cfg.RestartWork = launch[0].RestartWork
		cfg.Goals = launch[0].Goals
	}

	sup := New(cfg)
	// Publish the handle to the shutdown interrupter before Start can attach an RPC
	// transport. A drain racing this point either marks the supervisor interrupted
	// (so attach fails closed) or prevents registration and process launch entirely.
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil, errManagerStopping
	}
	m.starting[sessionID] = sup
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.starting[sessionID] == sup {
			delete(m.starting, sessionID)
		}
		m.mu.Unlock()
	}()
	// Startup is admitted by ctx but the successfully published supervisor lives
	// for the manager/daemon lifetime. Stop forwarding admission cancellation once
	// Start completes; until then either context interrupts dial/handshake/write.
	startCtx, cancelStart := context.WithCancel(m.supervisorCtx)
	stopAdmission := context.AfterFunc(ctx, cancelStart)
	if err := sup.Start(startCtx, wrappedArgv); err != nil {
		stopAdmission()
		cancelStart()
		return nil, err
	}
	if !stopAdmission() || ctx.Err() != nil || m.ctx.Err() != nil {
		cancelStart()
		_ = sup.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, m.ctx.Err()
	}
	_ = SaveIdentity(sup.Identity())

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		cancelStart()
		_ = sup.Close()
		return nil, errManagerStopping
	}
	m.sup[sessionID] = sup
	delete(m.starting, sessionID)
	m.mu.Unlock()
	return sup, nil
}

// RestartSnapshot includes headless supervisors and stays readable while their
// transports drain. It never performs RPC or starts/stops a runtime.
func (m *Manager) RestartSnapshot() map[string]RestartWork {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]RestartWork, len(m.sup))
	for id, s := range m.sup {
		if work := s.RestartWork(); work.ThreadID != "" {
			out[id] = work
		}
	}
	return out
}

// resumeThreadFor returns the thread to resume, or "" for an identity known never
// to have been persisted. Fresh threads are now persisted by the handshake before
// their first turn. A structured identity is authoritative when present. Without
// one, legacyThreadID lets the first structured launch adopt a conversation that
// the PTY path already pinned in the session store. Legacy sidecars without a
// version still resume. Every resume target must resume exactly; a failure
// fails the launch rather than substituting an empty thread.
func resumeThreadFor(sessionID, legacyThreadID string) string {
	id, ok := LoadIdentity(sessionID)
	if !ok {
		return legacyThreadID
	}
	if id.ThreadID == "" {
		return ""
	}
	switch {
	case id.Resumable:
		// Known to have a rollout (initialized, ran a turn, or resumed).
		return id.ThreadID
	case id.Version == 0:
		// Legacy identity persisted before Resumable existed — its history is
		// unknown, so it is resumed exactly and never silently discarded.
		return id.ThreadID
	default:
		// Current identity, known not-yet-run — start fresh to avoid a doomed resume.
		return ""
	}
}

// priorAccountsFor returns the ChatGPT accounts a resumed thread ran under,
// oldest first. A new process may continue the thread under the current
// central account; these remain in its identity as evidence.
func priorAccountsFor(sessionID, resumeThreadID string) []string {
	if resumeThreadID == "" {
		return nil
	}
	id, ok := LoadIdentity(sessionID)
	if !ok || id.ThreadID != resumeThreadID {
		return nil
	}
	prior := append([]string(nil), id.PriorChatGPTAccounts...)
	if id.ChatGPTAccount != "" {
		prior = append(prior, id.ChatGPTAccount)
	}
	return prior
}

// Close stops and forgets the supervisor for a session (its App Server exits). It
// leaves the persisted identity in place so a later Ensure resumes the same
// thread — archiving a session should not lose its conversation. Use Forget to
// drop the identity when a session is deleted for good. No-op for an unknown id.
func (m *Manager) Close(sessionID string) {
	m.close(sessionID, false)
}

func (m *Manager) close(sessionID string, forget bool) {
	gate := m.startGate(sessionID)
	<-gate
	defer func() { gate <- struct{}{} }()

	m.mu.Lock()
	s := m.sup[sessionID]
	delete(m.sup, sessionID)
	m.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
	if forget {
		_ = RemoveIdentity(sessionID)
	}
}

// Forget stops the supervisor (if any) and removes the persisted identity — for a
// session deleted for good, so no stale sidecar or event log lingers.
func (m *Manager) Forget(sessionID string) {
	m.close(sessionID, true)
}

// Shutdown stops every supervisor (daemon shutdown). Identities are left on disk
// so the next daemon run can resume them.
func (m *Manager) Shutdown() {
	m.stopContextWatch()
	m.mu.Lock()
	m.stopping = true
	all := make([]*Supervisor, 0, len(m.sup)+len(m.starting))
	seen := make(map[*Supervisor]struct{}, len(m.sup)+len(m.starting))
	for _, s := range m.sup {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			all = append(all, s)
		}
	}
	for _, s := range m.starting {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			all = append(all, s)
		}
	}
	m.sup = map[string]*Supervisor{}
	m.starting = map[string]*Supervisor{}
	m.mu.Unlock()
	m.cancelSupervisors()
	for _, s := range all {
		_ = s.Close()
	}
}

// InterruptTransports begins the manager's one-way shutdown drain and closes
// every live or in-progress supervisor transport. It deliberately does not kill
// App Server processes: Run invokes it immediately after context cancellation so
// blocked JSON-RPC I/O releases admission locks, joins those callers, and only
// then calls Shutdown for process teardown.
func (m *Manager) InterruptTransports() {
	m.mu.Lock()
	m.stopping = true
	all := make([]*Supervisor, 0, len(m.sup)+len(m.starting))
	seen := make(map[*Supervisor]struct{}, len(m.sup)+len(m.starting))
	for _, s := range m.sup {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			all = append(all, s)
		}
	}
	for _, s := range m.starting {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			all = append(all, s)
		}
	}
	m.mu.Unlock()

	for _, s := range all {
		s.interruptTransport()
	}
}

// Live reports the session ids with a running supervisor, for the rail's liveness
// annotation (a structured session has a supervisor, not an engine pane).
func (m *Manager) Live() map[string]bool {
	m.mu.Lock()
	out := make(map[string]bool, len(m.sup))
	for id, s := range m.sup {
		if s.live() {
			out[id] = true
		}
	}
	m.mu.Unlock()
	return out
}
