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

// Session is a normalized agent session surfaced from any Source.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`         // claude | hermes | tmux | workspace
	Kind      string `json:"kind"`           // agent kind, e.g. claude
	Mode      string `json:"mode,omitempty"` // task (short) | loop (long)
	Status    string `json:"status"`         // busy | idle | queued | ...
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
	Action string `json:"action"`         // attach | new | kill | resume | refresh
	ID     string `json:"id,omitempty"`   // target session id
	Kind   string `json:"kind,omitempty"` // for "new": agent kind to spawn
	Cwd    string `json:"cwd,omitempty"`  // for "new": working directory
}

// Result is the daemon -> client action response.
type Result struct {
	Type  string `json:"type"` // always "result"
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
