package codexapp

import (
	"context"
	"sync"
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
	ctx context.Context // daemon lifetime; every supervisor's Start is bound to it
	bin string          // codex binary override (AMUX_CODEX_BIN), "" ⇒ "codex"

	mu     sync.Mutex
	sup    map[string]*Supervisor
	starts map[string]*sync.Mutex // per-session creation lock (serialize Ensure before spawn)
}

// NewManager builds a Manager bound to the daemon's context. bin overrides the
// codex binary (pass the resolved AMUX_CODEX_BIN or "").
func NewManager(ctx context.Context, bin string) *Manager {
	return &Manager{ctx: ctx, bin: bin, sup: map[string]*Supervisor{}, starts: map[string]*sync.Mutex{}}
}

// startLock returns the per-session creation mutex, creating it once. Serializing
// Ensure per session BEFORE spawning is what prevents two callers from each
// launching an App Server on the same socket and the loser's cleanup unlinking the
// winner's listener (ROOT audit).
func (m *Manager) startLock(sessionID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.starts[sessionID]
	if l == nil {
		l = &sync.Mutex{}
		m.starts[sessionID] = l
	}
	return l
}

// Get returns the live supervisor for a session id, or false. It is the daemon's
// "is this session structured right now?" test as well as the route target.
func (m *Manager) Get(sessionID string) (*Supervisor, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sup[sessionID]
	return s, ok
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
// same value baked into wrappedArgv. Creation is serialized per session, so two
// callers never spawn competing servers.
func (m *Manager) Ensure(sessionID, dir string, env, wrappedArgv []string, endpoint string) (*Supervisor, error) {
	// Fast path: already live.
	if s, ok := m.Get(sessionID); ok {
		return s, nil
	}
	// Serialize creation for this session, then re-check under the lock so only one
	// caller ever spawns.
	lock := m.startLock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	if s, ok := m.Get(sessionID); ok {
		return s, nil
	}

	cfg := Config{
		SessionID:    sessionID,
		Bin:          m.bin,
		Dir:          dir,
		Env:          env,
		Endpoint:     endpoint,
		EventLogPath: EventLogPathFor(sessionID),
	}
	cfg.ResumeThreadID = resumeThreadFor(sessionID)

	sup := New(cfg)
	if err := sup.Start(m.ctx, wrappedArgv); err != nil {
		return nil, err
	}
	_ = SaveIdentity(sup.Identity())

	m.mu.Lock()
	m.sup[sessionID] = sup
	m.mu.Unlock()
	return sup, nil
}

// resumeThreadFor returns the thread id to resume for a session, or "" to start
// fresh. It resumes ONLY a persisted thread that has already run a turn
// (Resumable): attempting resume on a pinned-but-never-run thread returns "no
// rollout found" and can poison that thread's first turn (AGE-198), so a
// not-yet-resumable session starts fresh instead. The handshake keeps an error
// fallback for the rare case a rollout was pruned after being marked resumable.
func resumeThreadFor(sessionID string) string {
	id, ok := LoadIdentity(sessionID)
	if !ok || id.ThreadID == "" {
		return ""
	}
	switch {
	case id.Resumable:
		// Known to have a rollout (ran a turn, or was previously resumed).
		return id.ThreadID
	case id.Version == 0:
		// Legacy identity persisted before Resumable existed — it may hold a real
		// conversation, so don't silently discard it; attempt the resume and let the
		// handshake fall back to a fresh thread only on a genuine "no rollout" miss.
		return id.ThreadID
	default:
		// Current identity, known not-yet-run — start fresh to avoid a doomed resume.
		return ""
	}
}

// Close stops and forgets the supervisor for a session (its App Server exits). It
// leaves the persisted identity in place so a later Ensure resumes the same
// thread — archiving a session should not lose its conversation. Use Forget to
// drop the identity when a session is deleted for good. No-op for an unknown id.
func (m *Manager) Close(sessionID string) {
	m.mu.Lock()
	s := m.sup[sessionID]
	delete(m.sup, sessionID)
	m.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

// Forget stops the supervisor (if any) and removes the persisted identity — for a
// session deleted for good, so no stale sidecar or event log lingers.
func (m *Manager) Forget(sessionID string) {
	m.Close(sessionID)
	_ = RemoveIdentity(sessionID)
}

// Shutdown stops every supervisor (daemon shutdown). Identities are left on disk
// so the next daemon run can resume them.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := m.sup
	m.sup = map[string]*Supervisor{}
	m.mu.Unlock()
	for _, s := range all {
		_ = s.Close()
	}
}

// Live reports the session ids with a running supervisor, for the rail's liveness
// annotation (a structured session has a supervisor, not an engine pane).
func (m *Manager) Live() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool, len(m.sup))
	for id := range m.sup {
		out[id] = true
	}
	return out
}
