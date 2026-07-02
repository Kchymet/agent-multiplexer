package proto

import "encoding/json"

// RuntimeEvent is one structured transcript event derived from a runtime's
// on-disk session record (docs/remote-provider-sessions.md §4). The envelope is
// intentionally generic — a stable, documented vocabulary of Type strings, an
// optional coalescing ItemID, an optional Direction, and an opaque Payload — so
// amux carries no orchestrator-specific schema. Consumers MUST pass an unknown
// Type through rather than dropping it.
type RuntimeEvent struct {
	Type      string          `json:"type"`
	ItemID    string          `json:"item_id,omitempty"`
	Direction string          `json:"direction,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// RuntimeEventBatch groups a seq-ordered slice of RuntimeEvents with the ordinal
// of its last event. It is the internal handoff a runtime-events source hands the
// provider to frame — not itself a wire type (the provider unpacks it into a
// runtime-events HarnessMsg). Seq is per-session monotonic.
type RuntimeEventBatch struct {
	Seq    int64
	Events []RuntimeEvent
}

// Capabilities advertises what a provider can run, for orchestrator scheduling.
type Capabilities struct {
	MaxPanes int      `json:"maxPanes,omitempty"`
	Bwrap    bool     `json:"bwrap,omitempty"`
	OS       string   `json:"os,omitempty"`
	Arch     string   `json:"arch,omitempty"`
	Features []string `json:"features,omitempty"`
}

// PaneOffer is a still-running pane a reconnecting provider offers for resume.
// OutSeq is the last output frame the provider emitted (per-pane, monotonic).
type PaneOffer struct {
	PaneID  string `json:"paneId"`
	OutSeq  int64  `json:"outSeq,omitempty"`
	Running bool   `json:"running,omitempty"`
}

// AdoptPane is one entry in registered.adopt: the orchestrator adopts PaneID and
// wants output frames after AfterSeq retransmitted.
type AdoptPane struct {
	PaneID   string `json:"paneId"`
	AfterSeq int64  `json:"afterSeq,omitempty"`
}

// Session is the wire shape of an agent session published by the "sessions"
// feature (docs/remote-provider-sessions.md §2). It is a normalized, display-
// oriented snapshot row; the orchestrator renders it and issues lifecycle verbs
// against ID. This is the single canonical definition — amux's internal core
// aliases it.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`             // claude | hermes | workspace
	Kind      string `json:"kind"`               // agent kind, e.g. claude
	Mode      string `json:"mode,omitempty"`     // task (short) | loop (long)
	RootID    string `json:"rootId,omitempty"`   // parent root for sub-sessions
	IsRoot    bool   `json:"isRoot,omitempty"`   // true => a root container row
	Repos     string `json:"repos,omitempty"`    // agent rows: comma-joined repo names in scope
	Section   string `json:"section,omitempty"`  // rail grouping: workspaces | repos | detached
	State     string `json:"state,omitempty"`    // idle | ready | waiting | running
	Status    string `json:"status"`             // human label, e.g. "ready · main"
	Archived  bool   `json:"archived,omitempty"` // set on rows in the archived section
	Cwd       string `json:"cwd"`
	Pid       int    `json:"pid,omitempty"`
	StartedAt int64  `json:"startedAt"`
	CanAttach bool   `json:"canAttach"`
	CanKill   bool   `json:"canKill"`
	CanResume bool   `json:"canResume"`
}

// MuxMsg is an orchestrator -> provider message (v1: server -> harness).
type MuxMsg struct {
	Type    string   `json:"type"`
	Version int      `json:"version,omitempty"` // hello / registered: negotiated version
	PaneID  string   `json:"paneId,omitempty"`
	Dir     string   `json:"dir,omitempty"`  // spawn: working directory
	Env     []string `json:"env,omitempty"`  // spawn: KEY=VALUE additions
	Argv    []string `json:"argv,omitempty"` // spawn: command + args
	Cols    int      `json:"cols,omitempty"` // spawn/resize
	Rows    int      `json:"rows,omitempty"` // spawn/resize
	Data    []byte   `json:"data,omitempty"` // input bytes

	// v2 registered fields.
	OK               bool        `json:"ok,omitempty"`
	Error            string      `json:"error,omitempty"`            // registered: terminal reject reason
	ProviderID       string      `json:"providerId,omitempty"`       // registered: assigned id
	HeartbeatSeconds int         `json:"heartbeatSeconds,omitempty"` // registered: ping cadence
	GraceSeconds     int         `json:"graceSeconds,omitempty"`     // registered: pane survival after disconnect
	Adopt            []AdoptPane `json:"adopt,omitempty"`            // registered: resumed panes to replay
	Kill             []string    `json:"kill,omitempty"`             // registered: offered panes to terminate

	// v2 pong.
	T int64 `json:"t,omitempty"` // pong: echoes the ping timestamp

	// v2 "sessions" feature (session-action).
	ReqID  string            `json:"reqId,omitempty"`  // session-action: correlation id, echoed in the result
	Action string            `json:"action,omitempty"` // session-action: lifecycle verb
	ID     string            `json:"id,omitempty"`     // session-action: target session id
	Target string            `json:"target,omitempty"` // session-action: move destination (reserved)
	Fields map[string]string `json:"fields,omitempty"` // session-action: form fields (mirror the daemon's own clients)

	// v2 "runtime-events" feature (runtime-events-subscribe).
	SessionID string `json:"sessionId,omitempty"` // runtime-events-subscribe: the published session to stream
	AfterSeq  int64  `json:"afterSeq,omitempty"`  // runtime-events-subscribe: resume cursor (emit events with seq > afterSeq)
}

// HarnessMsg is a provider -> orchestrator message (v1: harness -> server).
type HarnessMsg struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"` // ready
	PaneID  string `json:"paneId,omitempty"`
	Data    []byte `json:"data,omitempty"`  // output bytes
	Error   string `json:"error,omitempty"` // exit

	// v2 fields.
	Seq          int64             `json:"seq,omitempty"`      // output/exit/reset: per-pane, monotonic from 1
	Versions     []int             `json:"versions,omitempty"` // register: protocol versions offered
	Token        string            `json:"token,omitempty"`    // register: bearer credential
	Name         string            `json:"name,omitempty"`     // register: provider display name
	Labels       map[string]string `json:"labels,omitempty"`   // register: scheduling labels
	Capabilities *Capabilities     `json:"capabilities,omitempty"`
	Panes        []PaneOffer       `json:"panes,omitempty"` // register: resumable panes
	T            int64             `json:"t,omitempty"`     // ping: timestamp

	// v2 "sessions" feature.
	Sessions []Session `json:"sessions,omitempty"` // sessions: full inventory snapshot (Seq is per-connection monotonic)
	ReqID    string    `json:"reqId,omitempty"`    // session-result: echoes the session-action reqId
	OK       bool      `json:"ok,omitempty"`       // session-result: verb succeeded
	NewID    string    `json:"newId,omitempty"`    // session-result: id of any session the verb created

	// v2 "runtime-events" feature (runtime-events frame). SessionID names the
	// published session; Seq is per-session monotonic (the ordinal of the last
	// event in Events); Events is a seq-ordered batch of structured transcript
	// events. A resuming consumer subscribes with afterSeq and receives only
	// events whose ordinal exceeds it.
	SessionID string         `json:"sessionId,omitempty"`
	Events    []RuntimeEvent `json:"events,omitempty"`
}
