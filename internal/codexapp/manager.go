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

	mu  sync.Mutex
	sup map[string]*Supervisor
}

// NewManager builds a Manager bound to the daemon's context. bin overrides the
// codex binary (pass the resolved AMUX_CODEX_BIN or "").
func NewManager(ctx context.Context, bin string) *Manager {
	return &Manager{ctx: ctx, bin: bin, sup: map[string]*Supervisor{}}
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
// fills the durable parts of the Config from the persisted identity (socket path
// and, when known, the thread to resume) and the per-session event log, starts the
// App Server bound to the daemon context, and persists the resulting identity so a
// later restart resumes the same thread. Idempotent: a second call returns the
// running supervisor unchanged.
//
// dir is the session's worktree; env are extra child-environment additions.
func (m *Manager) Ensure(sessionID, dir string, env []string) (*Supervisor, error) {
	m.mu.Lock()
	if s, ok := m.sup[sessionID]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	cfg := Config{
		SessionID:    sessionID,
		Bin:          m.bin,
		Dir:          dir,
		Env:          env,
		SocketPath:   SocketPathFor(sessionID),
		EventLogPath: EventLogPathFor(sessionID),
	}
	// Resume the same thread across daemon restarts when we have one on record.
	if id, ok := LoadIdentity(sessionID); ok && id.ThreadID != "" {
		cfg.ResumeThreadID = id.ThreadID
	}

	sup := New(cfg)
	if err := sup.Start(m.ctx); err != nil {
		return nil, err
	}
	_ = SaveIdentity(sup.Identity())

	m.mu.Lock()
	// A concurrent Ensure may have won the race while we were starting; if so, keep
	// the first and discard ours so there is only ever one server per session.
	if existing, ok := m.sup[sessionID]; ok {
		m.mu.Unlock()
		_ = sup.Close()
		return existing, nil
	}
	m.sup[sessionID] = sup
	m.mu.Unlock()
	return sup, nil
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
