// Package core holds the shared types and constants used across amux:
// the normalized Session model, the daemon<->client wire protocol, and the
// well-known names/paths that pin everything to the isolated tmux server.
package core

const (
	// TmuxSocket is the dedicated tmux socket label. Using `tmux -L amux`
	// gives us a server fully isolated from the user's default tmux server and
	// their ~/.tmux.conf.
	TmuxSocket = "amux"
	// SessionName is the single long-lived session inside the isolated server.
	SessionName = "main"
	// RailWidth is the column width of the persistent side-pane dashboard.
	RailWidth = 32
)

// Agent activity states, surfaced in Session.State. They form an attention
// ladder: a blocked agent (waiting) wants the user more than a working one.
const (
	StateIdle    = "idle"    // no live agent process (no tmux window)
	StateReady   = "ready"   // window live, turn finished, ready for the next message
	StateWaiting = "waiting" // window live, blocked on a prompt awaiting user input
	StateRunning = "running" // window live and the agent has an active turn
	// StateUnknown is a live window with no hook data yet (a session predating
	// the hooks, or one that hasn't fired its first event). Shown as a less
	// certain "running" so it reads as live without claiming granular knowledge.
	StateUnknown = "unknown"
)

// Rail sections, top to bottom (Session.Section). The console is sectionless and
// pinned above them all.
const (
	SectionWorkgroups = "workgroups" // cross-repo workgroups + nested agents
	SectionRepos      = "repos"      // tracked repos + their single-repo agents
	SectionDetached   = "detached"   // Claude sessions amux didn't launch
	SectionArchived   = "archived"   // agents marked done/archived (reversible)
)

// AgentSession is the dedicated, rail-free tmux session name that hosts one
// agent (keyed by its amux id). The native TUI attaches an embedded client to
// this session; because it's not shared, sizing/rendering stay clean.
func AgentSession(id string) string { return "amx-" + id }

// Session is a normalized agent session surfaced from any Source.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`            // claude | hermes | tmux | workspace
	Kind      string `json:"kind"`              // agent kind, e.g. claude
	Mode      string `json:"mode,omitempty"`    // task (short) | loop (long)
	RootID    string `json:"rootId,omitempty"`  // parent root for sub-sessions
	IsRoot    bool   `json:"isRoot,omitempty"`  // true => a root container row
	Section   string `json:"section,omitempty"` // rail grouping: workspaces | repos | detached
	State     string `json:"state,omitempty"`   // idle | ready | waiting | running
	Status    string `json:"status"`            // human label, e.g. "ready · main"
	Cwd       string `json:"cwd"`
	WindowID  string `json:"windowId"` // tmux @id if locally attached, else ""
	Pid       int    `json:"pid,omitempty"`
	StartedAt int64  `json:"startedAt"`
	CanAttach bool   `json:"canAttach"`
	CanKill   bool   `json:"canKill"`
	CanResume bool   `json:"canResume"`
}

// Snapshot is the daemon -> client state push (one JSON object per line).
type Snapshot struct {
	Type      string    `json:"type"` // always "snapshot"
	Sessions  []Session `json:"sessions"`
	UpdatedAt int64     `json:"updatedAt"`
}

// Action is the client -> daemon control request.
type Action struct {
	Action string            `json:"action"`           // attach | delete | refresh | new-repo-agent | add-agent | new-workgroup | add-repo | move | archive | rename
	ID     string            `json:"id,omitempty"`     // target session id (repo name for new-repo-agent; root id for add-agent)
	Kind   string            `json:"kind,omitempty"`   // for "new": agent kind to spawn
	Cwd    string            `json:"cwd,omitempty"`    // for "new": working directory
	Target string            `json:"target,omitempty"` // for "move": destination root id ("" = new work-scoped)
	Fields map[string]string `json:"fields,omitempty"` // form-driven actions (new-repo-agent, add-agent, new-workgroup, add-repo, rename)
}

// Result is the daemon -> client action response.
type Result struct {
	Type  string `json:"type"` // always "result"
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
