// Package core holds the shared types and constants used across amux:
// the normalized Session model, the daemon<->client wire protocol, and the
// well-known names/paths that pin everything to the daemon's engine.
package core

import (
	"encoding/json"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Session, the agent activity states, and the rail sections are the published
// wire subset: they live in the importable harnessproto module (the single source
// of truth the harness orchestrator imports instead of hand-mirroring) and are
// re-exported here so the rest of amux keeps referring to them as core.Session /
// core.State* / core.Section* unchanged.

// Session is a normalized agent session surfaced from any Source.
type Session = harnessproto.Session

// SessionCaps is the per-session control surface published alongside a Session
// (Session.Caps): the honest set of steering verbs the daemon can serve for it.
type SessionCaps = harnessproto.SessionCaps

// Agent activity states, surfaced in Session.State. They form an attention
// ladder: a blocked agent (waiting) wants the user more than a working one.
const (
	StateIdle    = harnessproto.StateIdle    // no live agent process
	StateReady   = harnessproto.StateReady   // live, turn finished, ready for the next message
	StateWaiting = harnessproto.StateWaiting // live, blocked on a prompt awaiting user input
	StateRunning = harnessproto.StateRunning // live and the agent has an active turn
	// StateUnknown is a live agent with no hook data yet (a session predating
	// the hooks, or one that hasn't fired its first event). Shown as a less
	// certain "running" so it reads as live without claiming granular knowledge.
	StateUnknown = harnessproto.StateUnknown
)

// Rail sections, top to bottom (Session.Section). The console is sectionless and
// pinned above them all.
const (
	SectionWorkgroups = harnessproto.SectionWorkgroups // cross-repo workgroups + nested agents
	SectionRepos      = harnessproto.SectionRepos      // tracked repos + their single-repo agents
	SectionDetached   = harnessproto.SectionDetached   // Claude sessions amux didn't launch
	SectionArchived   = harnessproto.SectionArchived   // agents marked done/archived (reversible)
)

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
	// ActionSteer drives the agent *inside* a running session rather than the
	// session itself: send it a prompt, interject mid-turn, interrupt the turn, or
	// answer its permission prompt. ID is the agent id and Fields[SteerVerb] names
	// which (SteerPrompt|SteerInterject|SteerStop|SteerPermission). Like
	// ActionStart it is engine-only — it changes no store state, so daemon.handle
	// serves it and it never reaches wsops.
	ActionSteer = "steer"
)

// ActionSteer field keys and the verbs they carry. They mirror the published
// wire vocabulary (harnessproto's steering verbs and field keys) so the provider
// maps a session-action onto a core.Action without inventing a second spelling,
// and `amux do steer` speaks the same words as the remote orchestrator.
const (
	SteerVerb      = "verb"       // which steering verb (Steer* below)
	SteerText      = "text"       // prompt/interject: the text to deliver
	SteerDecision  = "decision"   // permission: SteerAllow | SteerDeny
	SteerRequestID = "request_id" // permission: the request the decision answers; a stale one is refused
	SteerReason    = "reason"     // permission: optional free-text rationale

	SteerPrompt     = "prompt"     // deliver a new user turn
	SteerInterject  = "interject"  // deliver text while a turn is running
	SteerStop       = "stop"       // interrupt the turn without killing the session
	SteerPermission = "permission" // answer a pending permission prompt

	SteerAllow = "allow"
	SteerDeny  = "deny"
)

// Lifecycle action verbs (Action.Action values) — the control vocabulary the CLI,
// the TUI, the daemon, and the mux server all speak. Defined here, each with a
// descriptor (see actionDescriptors), so every dispatch path derives behavior
// from one table instead of re-deciding from raw strings and drifting — which
// they did: the CLI's archive once skipped the engine stop the TUI's performed.
const (
	ActionRefresh         = "refresh"          // re-poll only; no store change
	ActionSubscribe       = "subscribe"        // transport subscribe; a no-op in the store dispatch
	ActionDelete          = "delete"           // permanently remove a session (worktrees + branch)
	ActionKill            = "kill"             // alias of delete
	ActionArchive         = "archive"          // TUI one-key toggle: archive⇄restore
	ActionSetArchived     = "set-archived"     // CLI explicit archive/unarchive (Fields["archived"]=="true")
	ActionMove            = "move"             // re-parent an agent into another workgroup (Target)
	ActionRename          = "rename"           // set a session's display name (Fields["name"])
	ActionRmRepo          = "rm-repo"          // stop tracking a repo
	ActionAgentSetRepos   = "agent-set-repos"  // re-scope an agent to exactly Fields["repos"]
	ActionNewRepoAgent    = "new-repo-agent"   // create a repo-scoped workgroup + its agent
	ActionAddAgent        = "add-agent"        // add an agent to an existing workgroup
	ActionAddRepo         = "add-repo"         // track a new repo (Fields["source"])
	ActionNewWorkgroup    = "new-workgroup"    // create a work-scoped workgroup (+ optional first agent)
	ActionCreateWorkspace = "create-workspace" // CLI create: workgroup + optional default agent
)

// controlActions is the vocabulary a caller may drive from outside the UI —
// every lifecycle verb above plus "start" and "steer" (both engine-only). It is the single list
// behind the "valid actions" an unknown-verb error teaches, so the CLI and the
// daemon can never disagree about what `amux do` accepts. Order is the order we
// print: the everyday verbs first, then the create/attach ones. The pane.*
// streaming verbs and "query" are deliberately absent — they answer with frames
// a one-shot caller can't consume.
var controlActions = []string{
	ActionRefresh,
	ActionStart,
	ActionSteer,
	ActionRename,
	ActionMove,
	ActionArchive,
	ActionSetArchived,
	ActionDelete,
	ActionKill,
	ActionAddRepo,
	ActionRmRepo,
	ActionAgentSetRepos,
	ActionAddAgent,
	ActionNewRepoAgent,
	ActionNewWorkgroup,
	ActionCreateWorkspace,
}

// ControlActions returns the action verbs a caller may drive over the socket
// (`amux do <action>`), in print order. Callers get a copy, so an error message
// that annotates the list can't mutate the vocabulary.
func ControlActions() []string {
	out := make([]string, len(controlActions))
	copy(out, controlActions)
	return out
}

// KnownAction reports whether action is one of ControlActions().
func KnownAction(action string) bool {
	for _, a := range controlActions {
		if a == action {
			return true
		}
	}
	return false
}

// ActionDescriptor declares what a lifecycle verb does, so every dispatch path
// (daemon, mux server, CLI) reads one table instead of its own string switch.
type ActionDescriptor struct {
	// StopsEngine: applying this verb must stop the target's running process(es)
	// first — the daemon kills the engine instance, the mux server tears down the
	// agent's live panes — so a removed/archived session doesn't leak a PTY. For a
	// verb targeting a workgroup root the stop cascades to its agents. Unarchive
	// (also set-archived) targets an already-stopped session, so its stop is a
	// harmless no-op — the flag stays per-verb, not per-field.
	StopsEngine bool
	// CreatesSession: the verb creates a new session and returns its id in
	// Result.NewID, so a client can switch to (and start) it.
	CreatesSession bool
	// TargetsRoot: the created session (CreatesSession) is a workgroup root the
	// client resolves to its first agent, rather than an agent id directly.
	TargetsRoot bool
}

// actionDescriptors is the per-verb dispatch table. A verb absent here (refresh,
// move, rename, agent-set-repos, add-repo) has the zero descriptor: no engine
// stop, creates nothing.
var actionDescriptors = map[string]ActionDescriptor{
	ActionDelete:          {StopsEngine: true},
	ActionRmRepo:          {StopsEngine: true}, // the repo's home session (id = repo name) goes with it
	ActionKill:            {StopsEngine: true},
	ActionArchive:         {StopsEngine: true},
	ActionSetArchived:     {StopsEngine: true},
	ActionNewRepoAgent:    {CreatesSession: true},
	ActionAddAgent:        {CreatesSession: true},
	ActionNewWorkgroup:    {CreatesSession: true, TargetsRoot: true},
	ActionCreateWorkspace: {CreatesSession: true, TargetsRoot: true},
}

// DescriptorFor returns the dispatch descriptor for a lifecycle verb (the zero
// descriptor for verbs with no special dispatch behavior).
func DescriptorFor(action string) ActionDescriptor { return actionDescriptors[action] }

// Query names for ActionQuery — the read models the daemon serves so processes
// that don't own the store (the CLI and forms, and the provider peer) read
// through the daemon instead of opening SQLite themselves. (The daemon and the
// standalone mux server own store access directly; the CLI/provider do not.)
const (
	// QueryCodexControl reports the running daemon's startup selection.
	QueryCodexControl = "codex-control"
	QueryRepos        = "repos"    // tracked repositories -> []RepoRow
	QuerySessions     = "sessions" // workgroups + their agents -> []WorkgroupRow
	// QuerySnapshot returns the daemon's current session rail ([]Session) — the same
	// inventory it broadcasts to subscribers — so a peer process (the provider)
	// publishes it without opening the store itself.
	QuerySnapshot = "snapshot"
	// QueryRuntimePath resolves Action.ID (a session id) to the on-disk transcript
	// path a runtime-event stream tails, as a JSON string ("" when the session has
	// no supported record). Lets the provider resolve transcripts via the daemon
	// instead of reading the store directly.
	QueryRuntimePath = "runtime-path"
	// QueryRuntimeRecord is QueryRuntimePath plus the runtime that wrote the
	// record (-> RuntimeRecord), so the provider picks the right reader and can
	// tell the orchestrator which runtime a session's events came from. Path is ""
	// when the session has no supported record.
	QueryRuntimeRecord = "runtime-record"
	// QueryVersion reports the daemon build, its CLI protocol, and the schema
	// version of the database it owns. It is intentionally additive: a new CLI
	// can identify an older daemon by the latter's unknown-query response.
	QueryVersion = "version"
)

// VersionInfo is the QueryVersion read model. Protocol compatibility is a
// CLI-to-daemon concern; the schema range states what the reporting daemon can
// safely open. Product version strings are diagnostic rather than compatibility
// gates, since patch/minor releases may retain the same contracts.
type VersionInfo struct {
	DaemonVersion     string `json:"daemonVersion"`
	DaemonProtocol    int    `json:"daemonProtocol"`
	DatabaseSchema    int    `json:"databaseSchema"`
	DatabaseMinSchema int    `json:"databaseMinSchema"`
	DatabaseMaxSchema int    `json:"databaseMaxSchema"`
	DatabaseError     string `json:"databaseError,omitempty"`
}

// RuntimeRecord is the QueryRuntimeRecord read model: a session's on-disk runtime
// transcript and the runtime (agent kind) that wrote it. A zero Path means the
// session has no transcript a runtime-event reader supports — it may still carry
// a Journal, which is all a never-started session has.
type RuntimeRecord struct {
	Runtime string `json:"runtime,omitempty"`
	Path    string `json:"path,omitempty"`
	// Permissions is amux's own permission journal for the session, when its
	// runtime resolves permission prompts without recording them (Claude Code).
	// The reader tails it alongside Path so the prompts reach the stream at all;
	// empty for a runtime that records its own (Codex). It is additive: a client
	// older than the field simply reads the transcript alone.
	Permissions string `json:"permissions,omitempty"`
	// Journal is amux's own session journal (journal.go): what amux did to the
	// session, which no runtime records. The reader tails it alongside Path, and
	// it is resolvable for a session with no transcript at all — which is the
	// point, since that is the stopped agent an accepted `prompt` is starting.
	Journal string `json:"journal,omitempty"`
	// Structured marks Path as a supervisor-written event record whose lines are
	// already normalized runtime events (AGE-181, structured control mode), so the
	// reader decodes them with the identity mapper. Additive/omitempty: a client
	// older than the field reads Path as a raw runtime transcript, as before.
	Structured bool `json:"structured,omitempty"`
}

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
	Agents  []AgentRow `json:"agents"`
	// Role is the root's own session role: coordinator for a work-scoped
	// workgroup, repo for a repo's home session, "" for a hidden single-member
	// repo root (which hosts no session). Dir is that session's sandbox (the
	// container dir), "" when the root hosts none or predates default sessions.
	Role  string `json:"role,omitempty"`
	Agent string `json:"agent,omitempty"` // the root session's runtime kind
	Dir   string `json:"dir,omitempty"`
}

// AgentRow is one agent (child session) under a WorkgroupRow.
type AgentRow struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`         // claude | hermes
	Mode     string `json:"mode"`          // task | interactive
	Repos    string `json:"repos"`         // comma-joined repo names
	Branch   string `json:"branch"`        // the agent's git branch (for doctor reconciliation)
	Dir      string `json:"dir,omitempty"` // the agent's sandbox dir (holds its worktrees and private config homes)
	Archived bool   `json:"archived"`
}
