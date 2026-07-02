// Package harnessproto is the Multiplexer Server <-> Agent Harness wire protocol:
// the server tells a harness to spawn/feed/kill PTY-backed panes, and the harness
// streams their output back. See docs/client-server.md. One JSON object per line;
// pane bytes ride in Data ([]byte, base64-encoded by encoding/json).
package harnessproto

import (
	"crypto/subtle"
	"encoding/json"
	"io"

	"amux/internal/core"
	"amux/internal/wire"
)

// Version is the current protocol version. v1 is the in-process/stdio harness
// handshake (hello/ready, no auth, no seq). v2 is the additive remote-provider
// extension (register/registered, ping/pong, per-pane monotonic output seq, and
// reset for replay-buffer overflow); see docs/remote-provider.md.
const (
	Version = 1
	// Version2 is the remote-provider protocol. The two are wire-compatible: a v2
	// peer opens with register/registered instead of hello/ready.
	Version2 = 2
)

// Server -> harness message types. In v2 the "server" role is the remote
// orchestrator and these travel orchestrator -> provider.
const (
	MHello      = "hello"
	MSpawn      = "spawn"
	MInput      = "input"
	MResize     = "resize"
	MKill       = "kill"
	MRegistered = "registered" // v2: accept/reject a register, negotiate version, resolve resume
	MPong       = "pong"       // v2: heartbeat reply

	// v2 "sessions" feature (opt-in, see docs/remote-provider-sessions.md):
	// orchestrator -> provider. Never sent unless the provider advertised the
	// "sessions" feature in register.
	MSessionsSubscribe = "sessions-subscribe" // begin receiving the provider's session inventory
	MSessionAction     = "session-action"     // a session lifecycle verb to execute

	// v2 "runtime-events" feature (opt-in, docs/remote-provider-sessions.md §4):
	// orchestrator -> provider. Never sent unless the provider advertised
	// "runtime-events". The orchestrator subscribes per published session (and
	// resumes from afterSeq); read-only — there is no input counterpart.
	MRuntimeEventsSubscribe = "runtime-events-subscribe" // begin receiving a session's structured transcript events
)

// Harness -> server message types. In v2 the "harness" role is the provider and
// these travel provider -> orchestrator.
const (
	HReady    = "ready"
	HOutput   = "output"
	HExit     = "exit"
	HRegister = "register" // v2: first frame — offer versions, token, caps, resumable panes
	HReset    = "reset"    // v2: replay buffer overflowed; frames before Seq are gone
	HPing     = "ping"     // v2: heartbeat

	// v2 "sessions" feature: provider -> orchestrator.
	HSessions      = "sessions"       // full session-inventory snapshot (replaces the previous one)
	HSessionResult = "session-result" // result of a session-action

	// v2 "runtime-events" feature: provider -> orchestrator. A batch of structured
	// transcript events for one published session, seq-ordered (docs/
	// remote-provider-sessions.md §4).
	HRuntimeEvents = "runtime-events"
)

// SessionsFeature is the feature string a provider advertises in
// register.capabilities.features to opt into publishing its session inventory
// and accepting session lifecycle verbs (docs/remote-provider-sessions.md §1).
const SessionsFeature = "sessions"

// RuntimeEventsFeature is the feature string a provider advertises to opt into
// streaming structured transcript events for the sessions it publishes
// (docs/remote-provider-sessions.md §1/§4). It is independent of and additive to
// "sessions": a provider may advertise "sessions" alone (status-only inventory).
const RuntimeEventsFeature = "runtime-events"

// Session lifecycle verbs the "sessions" feature accepts (spec §3). Anything
// else — including any pane/terminal verb — is rejected with
// session-result{ok:false,error:"unsupported"}.
const (
	VerbNewWorkgroup = "new-workgroup"
	VerbAddAgent     = "add-agent"
	VerbRename       = "rename"
	VerbArchive      = "archive"
	VerbUnarchive    = "unarchive"
	VerbStart        = "start"
)

// ErrUnsupported is the session-result error for a verb the provider does not
// accept (spec §3).
const ErrUnsupported = "unsupported"

// Terminal registration errors (MuxMsg.Error on a rejected registered): the
// provider exits with the message instead of retrying.
const (
	ErrBadToken   = "bad-token"
	ErrRevoked    = "revoked"
	ErrBadVersion = "unsupported-version"
)

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

// MuxMsg is a server -> harness message (v2: orchestrator -> provider).
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

// HarnessMsg is a harness -> server message (v2: provider -> orchestrator).
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
	Sessions []core.Session `json:"sessions,omitempty"` // sessions: full inventory snapshot (Seq is per-connection monotonic)
	ReqID    string         `json:"reqId,omitempty"`    // session-result: echoes the session-action reqId
	OK       bool           `json:"ok,omitempty"`       // session-result: verb succeeded
	NewID    string         `json:"newId,omitempty"`    // session-result: id of any session the verb created

	// v2 "runtime-events" feature (runtime-events frame). SessionID names the
	// published session; Seq is per-session monotonic (the ordinal of the last
	// event in Events); Events is a seq-ordered batch of structured transcript
	// events. A resuming consumer subscribes with afterSeq and receives only
	// events whose ordinal exceeds it.
	SessionID string         `json:"sessionId,omitempty"`
	Events    []RuntimeEvent `json:"events,omitempty"`
}

// Conn is a typed harnessproto connection.
type Conn struct{ w *wire.Conn }

// NewConn wraps a stream for harnessproto.
func NewConn(rwc io.ReadWriteCloser) *Conn { return &Conn{w: wire.New(rwc)} }

func (c *Conn) Close() error { return c.w.Close() }

// Server-side helpers.
func (c *Conn) WriteMux(m MuxMsg) error { return c.w.Write(m) }
func (c *Conn) ReadHarness() (HarnessMsg, error) {
	var m HarnessMsg
	err := c.w.Read(&m)
	return m, err
}

// Harness-side helpers.
func (c *Conn) WriteHarness(m HarnessMsg) error { return c.w.Write(m) }
func (c *Conn) ReadMux() (MuxMsg, error) {
	var m MuxMsg
	err := c.w.Read(&m)
	return m, err
}

// TokenOK reports whether a registering provider's token authenticates against
// the configured one, in constant time. An empty configured token disables auth.
// Used by the orchestrator (server) side of a v2 registration.
func TokenOK(configured, presented string) bool {
	if configured == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(configured), []byte(presented)) == 1
}

// Negotiate picks the highest version common to the provider's offer and the
// versions the orchestrator supports, or ok=false when there is no overlap
// (fail loudly — the caller replies with ErrBadVersion). Supported must be
// sorted ascending or unordered; the max common value wins regardless.
func Negotiate(offered, supported []int) (version int, ok bool) {
	for _, o := range offered {
		for _, s := range supported {
			if o == s && o > version {
				version, ok = o, true
			}
		}
	}
	return version, ok
}
