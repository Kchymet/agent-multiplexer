// Package daemonstatus is the daemon's lifecycle progress channel: a small
// status file next to the singleton lock that the daemon rewrites while it
// starts up and shuts down, and that `amux daemon stop|start|restart` read to
// show what the daemon is waiting on.
//
// A file rather than a stream on the shutdown RPC connection, because neither
// window the client needs to observe has a usable connection: shutdown drains
// every accepted connection (the shutdown caller's included) before it stops
// any agent, and startup reconciles every session mailbox before it listens at
// all. The file is written only by the process that holds the singleton lock
// and removed before that lock is released, so the lock orders its owners: a
// replacement daemon cannot start writing until the old one has finished. It is
// advisory only: lifecycle correctness still rests on the socket probe and the
// lock, so a stale or unreadable file can make output less informative but
// never changes whether a stop or start succeeds.
package daemonstatus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
)

// Operations.
const (
	OpStarting = "starting"
	OpStopping = "stopping"
)

// Phases, in the order a daemon passes through them.
const (
	PhaseReconciling = "reconciling" // startup: adopting session mailboxes
	PhaseListening   = "listening"   // startup: opening the host socket
	PhaseDraining    = "draining"    // shutdown: closing connections and in-flight work
	PhaseCodex       = "codex"       // shutdown: stopping structured Codex sessions
	PhaseAgents      = "agents"      // shutdown: waiting for agents, then SIGTERM/SIGKILL
	PhaseReleasing   = "releasing"   // shutdown: socket, authority, singleton lock
)

// Agent instance states during PhaseAgents.
const (
	AgentBusy        = "busy"        // mid-turn; waiting for it to reach a safe point
	AgentTerminating = "terminating" // SIGTERM sent; waiting for it to flush and exit
	AgentKilling     = "killing"     // ignored SIGTERM; SIGKILL sent
	AgentStopped     = "stopped"
)

// Agent is one engine instance the daemon is still accounting for.
type Agent struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Tab   int    `json:"tab,omitempty"`
	State string `json:"state"`
	// Until is when this instance escalates (busy→SIGTERM, SIGTERM→SIGKILL),
	// as Unix milliseconds; zero when no escalation is pending.
	Until int64 `json:"until,omitempty"`
}

// Status is the file's whole content.
type Status struct {
	PID     int    `json:"pid"`
	Op      string `json:"op"`
	Phase   string `json:"phase"`
	Started int64  `json:"started"` // Unix ms the operation began
	Updated int64  `json:"updated"` // Unix ms of this write
	// Done/Total count progress within a countable phase (mailboxes).
	Done   int     `json:"done,omitempty"`
	Total  int     `json:"total,omitempty"`
	Agents []Agent `json:"agents,omitempty"`
}

// Path is where the running daemon publishes its status.
func Path() string { return filepath.Join(core.StateDir(), "daemon-status.json") }

// Read loads a status file. ok is false when it is absent or unreadable.
func Read(path string) (Status, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, false
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil || s.Op == "" {
		return Status{}, false
	}
	return s, true
}

// write replaces path atomically so a reader never sees a torn file.
func write(path string, s Status) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-status-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Reporter publishes one daemon's status. A nil *Reporter is a no-op, so
// daemons built without one (tests) need no guards. Writes are best effort:
// progress output is advisory and must never fail a start or a stop.
type Reporter struct {
	mu   sync.Mutex
	path string
	pid  int
	now  func() time.Time
	cur  Status
	done bool
}

// NewReporter returns a reporter writing to path for this process.
func NewReporter(path string) *Reporter {
	return &Reporter{path: path, pid: os.Getpid(), now: time.Now}
}

// Begin starts a new operation, discarding per-phase detail.
func (r *Reporter) Begin(op, phase string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cur = Status{PID: r.pid, Op: op, Phase: phase, Started: r.now().UnixMilli()}
	r.done = false
	r.flushLocked()
}

// Phase moves the current operation to phase, clearing counters and agents.
func (r *Reporter) Phase(phase string) {
	r.update(func(s *Status) {
		s.Phase, s.Done, s.Total, s.Agents = phase, 0, 0, nil
	})
}

// Count records progress within a countable phase.
func (r *Reporter) Count(done, total int) {
	r.update(func(s *Status) { s.Done, s.Total = done, total })
}

// Agents records the instances the current phase is waiting on.
func (r *Reporter) Agents(agents []Agent) {
	sorted := append([]Agent(nil), agents...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].ID != sorted[j].ID {
			return sorted[i].ID < sorted[j].ID
		}
		return sorted[i].Tab < sorted[j].Tab
	})
	r.update(func(s *Status) { s.Agents = sorted })
}

// Clear removes the status file. Call it while still holding the singleton
// lock: once the lock is released the path belongs to the next daemon. A
// cleared reporter ignores later updates.
func (r *Reporter) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	_ = os.Remove(r.path)
}

func (r *Reporter) update(f func(*Status)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur.Op == "" || r.done {
		return
	}
	f(&r.cur)
	r.flushLocked()
}

func (r *Reporter) flushLocked() {
	r.cur.Updated = r.now().UnixMilli()
	_ = write(r.path, r.cur)
}

// Summary renders s as one human line, e.g.
//
//	waiting on 2 agents: 1 busy, 1 terminating — SIGTERM in 12s · fix-login (busy), api (terminating)
//
// Times are relative to now. It never includes the overall elapsed/remaining
// budget; callers that know their own budget add it.
func Summary(s Status, now time.Time) string {
	switch s.Phase {
	case PhaseReconciling:
		if s.Total > 0 {
			return fmt.Sprintf("reconciling session mailboxes %d/%d", s.Done, s.Total)
		}
		return "reconciling session mailboxes"
	case PhaseListening:
		return "opening control socket"
	case PhaseDraining:
		return "closing client connections and in-flight work"
	case PhaseCodex:
		return "stopping structured Codex sessions"
	case PhaseReleasing:
		return "releasing control socket and singleton lock"
	case PhaseAgents:
		return agentsSummary(s.Agents, now)
	}
	return s.Phase
}

func agentsSummary(agents []Agent, now time.Time) string {
	counts := map[string]int{}
	var waiting []Agent
	var soonest int64
	for _, a := range agents {
		if a.State == AgentStopped {
			continue
		}
		counts[a.State]++
		waiting = append(waiting, a)
		if a.Until > 0 && (soonest == 0 || a.Until < soonest) {
			soonest = a.Until
		}
	}
	if len(waiting) == 0 {
		if len(agents) == 0 {
			return "stopping agents"
		}
		return fmt.Sprintf("all %d agents stopped", len(agents))
	}
	var parts []string
	for _, state := range []string{AgentBusy, AgentTerminating, AgentKilling} {
		if n := counts[state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	noun := "agents"
	if len(waiting) == 1 {
		noun = "agent"
	}
	line := fmt.Sprintf("waiting on %d %s (%s)", len(waiting), noun, strings.Join(parts, ", "))
	if soonest > 0 {
		next := "SIGKILL"
		for _, a := range waiting {
			if a.Until == soonest && a.State == AgentBusy {
				next = "SIGTERM"
			}
		}
		left := time.UnixMilli(soonest).Sub(now)
		if left < 0 {
			left = 0
		}
		line += fmt.Sprintf(" — %s in %s", next, left.Round(time.Second))
	}
	names := make([]string, 0, len(waiting))
	for _, a := range waiting {
		names = append(names, fmt.Sprintf("%s (%s)", a.Label(), a.State))
	}
	return line + ": " + strings.Join(names, ", ")
}

// Label names an instance: its title, falling back to its id, with the tab
// for non-agent panes.
func (a Agent) Label() string {
	label := a.Title
	if label == "" {
		label = a.ID
	}
	if a.Tab != 0 {
		label = fmt.Sprintf("%s [tab %d]", label, a.Tab)
	}
	return label
}

// Key identifies what changed between two statuses for output that must not
// repeat itself (plain, non-terminal lines): the phase, counters and each
// agent's state, but not the ticking deadlines or mailbox counter.
func Key(s Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%s|%s|%d", s.PID, s.Op, s.Phase, s.Total)
	for _, a := range s.Agents {
		fmt.Fprintf(&b, "|%s:%d=%s", a.ID, a.Tab, a.State)
	}
	return b.String()
}
