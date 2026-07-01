// Package core holds the shared types and constants used across amux:
// the normalized Session model, the daemon<->client wire protocol, and the
// well-known names/paths that pin everything to the daemon's engine.
package core

import "encoding/json"

// Agent activity states, surfaced in Session.State. They form an attention
// ladder: a blocked agent (waiting) wants the user more than a working one.
const (
	StateIdle    = "idle"    // no live agent process
	StateReady   = "ready"   // live, turn finished, ready for the next message
	StateWaiting = "waiting" // live, blocked on a prompt awaiting user input
	StateRunning = "running" // live and the agent has an active turn
	// StateUnknown is a live agent with no hook data yet (a session predating
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

// Session is a normalized agent session surfaced from any Source.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`            // claude | hermes | workspace
	Kind      string `json:"kind"`              // agent kind, e.g. claude
	Mode      string `json:"mode,omitempty"`    // task (short) | loop (long)
	RootID    string `json:"rootId,omitempty"`  // parent root for sub-sessions
	IsRoot    bool   `json:"isRoot,omitempty"`  // true => a root container row
	Section   string `json:"section,omitempty"` // rail grouping: workspaces | repos | detached
	State     string `json:"state,omitempty"`   // idle | ready | waiting | running
	Status    string `json:"status"`            // human label, e.g. "ready · main"
	Cwd       string `json:"cwd"`
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

// Pane action verbs (Action.Action). Unlike the lifecycle verbs these are
// streamed per connection: the daemon attaches the connection to an engine-owned
// agent pane and routes its I/O. Detaching (PaneClose / disconnect) never stops
// the agent — only delete/archive do.
const (
	ActionPaneOpen   = "pane.open"   // start streaming a tab of an agent (ID=agent id, Tab, PaneID, Cols, Rows)
	ActionPaneInput  = "pane.input"  // forward input bytes to a pane (PaneID, Data)
	ActionPaneResize = "pane.resize" // resize a pane (PaneID, Cols, Rows)
	ActionPaneClose  = "pane.close"  // detach a pane (PaneID) — does not kill the agent
)

// Lifecycle verbs that aren't pane.* streaming. Most are handled by
// wsops.ApplyResult; "start" is engine-only (no store change): it launches an
// agent's process in the daemon's engine without attaching a UI, so a
// CLI-created session comes up running the way the TUI starts one on open.
const (
	ActionStart = "start" // ensure an agent's (or a root's agents') process is running (ID=agent or root id)
	ActionQuery = "query" // read a store-backed model over the socket (Query names it); the daemon replies with a Data frame
)

// Query names for ActionQuery — the read models the daemon serves so clients
// (the CLI, forms) never open the store themselves.
const (
	QueryRepos    = "repos"    // tracked repositories -> []RepoRow
	QuerySessions = "sessions" // workgroups + their agents -> []WorkgroupRow
)

// Action is the client -> daemon control request. It carries both the lifecycle
// verbs and the pane.* streaming verbs; the pane fields apply only to the latter.
type Action struct {
	Action string            `json:"action"`           // lifecycle verb (attach|delete|…) or a pane.* verb
	ID     string            `json:"id,omitempty"`     // target session id (repo name for new-repo-agent; root id for add-agent; agent id for pane.open)
	Kind   string            `json:"kind,omitempty"`   // for "new": agent kind to spawn
	Cwd    string            `json:"cwd,omitempty"`    // for "new": working directory
	Target string            `json:"target,omitempty"` // for "move": destination root id ("" = new work-scoped)
	Query  string            `json:"query,omitempty"`  // for ActionQuery: which read model to return (QueryRepos|QuerySessions)
	Fields map[string]string `json:"fields,omitempty"` // form-driven actions (new-repo-agent, add-agent, new-workgroup, add-repo, rename)

	// Pane streaming fields (pane.* verbs only).
	PaneID string `json:"paneId,omitempty"` // client-minted stream id, scoped to the connection
	Tab    int    `json:"tab,omitempty"`    // pane.open: which tab (agent|editor|terminal)
	Cols   int    `json:"cols,omitempty"`   // pane.open/resize
	Rows   int    `json:"rows,omitempty"`   // pane.open/resize
	Data   []byte `json:"data,omitempty"`   // pane.input bytes (base64 over JSON)
}

// Result is the daemon -> client action response.
type Result struct {
	Type  string `json:"type"` // always "result"
	OK    bool   `json:"ok"`
	NewID string `json:"newId,omitempty"` // id of a session the action created (so a client can switch to it)
	Error string `json:"error,omitempty"`
}

// Pane frame types (PaneFrame.Type), streamed daemon -> client for an attached
// pane.
const (
	FramePaneOutput = "pane.output" // pane produced output (PaneID, Data)
	FramePaneExit   = "pane.exit"   // pane's process ended (PaneID, Error)
	FramePaneReset  = "pane.reset"  // client must reset its emulator before the next output (PaneID)
)

// PaneFrame is a daemon -> client message carrying one agent pane's output or its
// exit. PaneID echoes the id the client minted in pane.open.
type PaneFrame struct {
	Type   string `json:"type"`
	PaneID string `json:"paneId"`
	Data   []byte `json:"data,omitempty"`  // pane.output bytes (base64 over JSON)
	Error  string `json:"error,omitempty"` // pane.exit error, if any
}

// FrameData is the daemon -> client reply to an ActionQuery: the requested read
// model, marshalled as Rows. The CLI decodes Rows into the matching row type
// (RepoRow / WorkgroupRow) so it never opens the store itself.
const FrameData = "data"

// Data carries one query's result. Query echoes the request; on success Rows
// holds the JSON-encoded slice, otherwise Error explains the failure.
type Data struct {
	Type  string          `json:"type"`  // always FrameData
	Query string          `json:"query"` // echoes Action.Query
	OK    bool            `json:"ok"`
	Rows  json.RawMessage `json:"rows,omitempty"`
	Error string          `json:"error,omitempty"`
}

// RepoRow is one tracked repository, the QueryRepos element type. It's the
// display subset of store.Repo — the CLI only prints name and source.
type RepoRow struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

// WorkgroupRow is one workgroup root plus its agents, the QuerySessions element
// type. It flattens the store's root/child rows into what `session ls` prints,
// so the CLI renders the listing without a store handle.
type WorkgroupRow struct {
	ID      string     `json:"id"`
	Scope   string     `json:"scope"`   // work | repo
	Display string     `json:"display"` // name if set, else id
	Repos   string     `json:"repos"`   // comma-joined repo names
	Agents  []AgentRow `json:"agents"`
}

// AgentRow is one agent (child session) under a WorkgroupRow.
type AgentRow struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"` // claude | hermes
	Mode     string `json:"mode"`  // task | loop
	Repos    string `json:"repos"` // comma-joined repo names
	Archived bool   `json:"archived"`
}
