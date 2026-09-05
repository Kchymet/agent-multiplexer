// Package harnessproto is the Multiplexer Server <-> Agent Harness wire protocol:
// the server tells a harness to spawn/feed/kill PTY-backed panes, and the harness
// streams their output back. See docs/client-server.md. One JSON object per line;
// pane bytes ride in Data ([]byte, base64-encoded by encoding/json).
//
// This is the published source of truth for the wire. It lives in its own nested
// module (github.com/kchymet/agent-multiplexer/harnessproto) with a stdlib-only
// dependency surface so external consumers — notably the harness orchestrator in
// a separate repo — import these exact types, tags, verbs, and vocabulary rather
// than maintaining a byte-for-byte hand copy. Renaming a JSON tag here changes the
// golden fixtures in golden_test.go and breaks this module's own tests, so drift
// fails PR CI in the repo that owns the wire.
package harnessproto

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"io"
	"sync"
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

// Session steering verbs (spec §3.1): they act on the agent *inside* a running
// session rather than on the session's lifecycle. They are still session verbs —
// they carry no pane access, and the daemon decides how to deliver them.
//
//   - VerbPrompt delivers a new user turn to the session's agent. FieldText
//     carries the text. If the agent is not running the daemon MAY start it with
//     the text as its initial prompt, and MAY answer before that start finishes
//     (ResultAccepted) rather than holding the caller for a runtime cold start.
//   - VerbInterject delivers text to the agent while a turn is already running
//     (a steer, not a new turn). FieldText carries the text.
//   - VerbStop interrupts the current turn without killing the session. It
//     takes no fields; the session stays alive and ready for the next verb.
//   - VerbPermission resolves a permission request the runtime surfaced on the
//     runtime-events stream as a TypePermissionRequest event (§4).
//     FieldRequestID echoes that event's request_id, FieldDecision is
//     DecisionAllow or DecisionDeny, and FieldReason is optional free text.
//     FieldRequestID is correlated, not decorative: a daemon MUST refuse an id
//     that does not name the request the runtime currently has open (the prompt
//     it named has since been answered, so the keystroke would land on a
//     different one) rather than answering blind.
//
// Steering is inherently asynchronous: a successful result means the daemon
// accepted the verb (ResultAccepted), not that the agent has finished acting on
// it — and, for a VerbPrompt that has to start a stopped agent, not even that it
// has been delivered yet. See HarnessMsg.Result and HarnessMsg.Accepted.
const (
	VerbPrompt     = "prompt"
	VerbInterject  = "interject"
	VerbStop       = "stop"
	VerbPermission = "permission"
)

// SessionVerbs is the closed set of accepted session-action verbs. A consumer
// (the orchestrator) uses it to reject a pane/terminal verb before a wasted
// round-trip; the provider rejects anything outside it with ErrUnsupported. A
// verb *in* this set that a particular daemon does not implement is rejected
// with ErrUnsupportedVerb instead — the distinction is how an orchestrator tells
// "never valid" from "this daemon is older than this verb" (spec §3.2).
var SessionVerbs = map[string]bool{
	VerbNewWorkgroup: true,
	VerbAddAgent:     true,
	VerbRename:       true,
	VerbArchive:      true,
	VerbUnarchive:    true,
	VerbStart:        true,
	VerbPrompt:       true,
	VerbInterject:    true,
	VerbStop:         true,
	VerbPermission:   true,
}

// SteeringVerbs is the subset of SessionVerbs that steers the agent inside a
// running session (as opposed to managing the session's lifecycle). A daemon
// publishing read-only rejects these exactly as it rejects the lifecycle verbs;
// the set exists so both peers can name the group without re-listing it.
var SteeringVerbs = map[string]bool{
	VerbPrompt:     true,
	VerbInterject:  true,
	VerbStop:       true,
	VerbPermission: true,
}

// session-action field keys (MuxMsg.Fields) for the steering verbs. Lifecycle
// verbs mirror the daemon's own form fields and have no published key set; these
// four are protocol, so both peers spell them the same way.
const (
	FieldText      = "text"       // prompt / interject: the text to deliver
	FieldRequestID = "request_id" // permission: the request_id from the permission_request event
	FieldDecision  = "decision"   // permission: DecisionAllow | DecisionDeny
	FieldReason    = "reason"     // permission: optional free-text rationale
)

// Permission decisions (the FieldDecision value on a VerbPermission action).
// Anything else is rejected: the daemon must never guess at an ambiguous
// decision on a permission prompt.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// DecisionCleared is the resolution a TypePermissionResolved event carries when
// the producer knows the prompt closed but not which way it went (the turn
// ended, the session exited). It is NOT a decision a caller may send on a
// VerbPermission action — the daemon accepts only DecisionAllow / DecisionDeny.
const DecisionCleared = "cleared"

// session-result dispositions (HarnessMsg.Result), distinguishing a verb whose
// effect is already observable from one that was merely delivered. Absent (the
// empty string) means ResultApplied — every lifecycle verb predating this field
// is synchronous, so an older daemon's bare {ok:true} reads correctly.
const (
	// ResultApplied: the verb ran to completion and its effect is in the next
	// sessions snapshot. The lifecycle verbs are all applied.
	ResultApplied = "applied"
	// ResultAccepted: the verb was validated and taken on, and its effect is
	// asynchronous — watch the runtime-events stream (or the session's state) for
	// it. The steering verbs are accepted, not applied.
	//
	// Accepted deliberately promises less than "delivered": a `prompt` to a
	// stopped agent is acknowledged as soon as it is known to be deliverable, and
	// the daemon then cold-starts the runtime (seconds) and types the text in
	// afterwards, reporting its progress as `notice` events on the session. A
	// daemon that answered only once the agent was up would blow past the
	// client-side dispatch timeouts sitting in front of this relay for the most
	// ordinary case there is — prompting an idle session.
	ResultAccepted = "accepted"
)

// ErrUnsupported is the session-result error for a verb the provider does not
// accept (spec §3) — one outside SessionVerbs, including every pane/terminal
// verb. It means "never valid on this protocol", so an orchestrator should not
// retry it against another daemon.
const ErrUnsupported = "unsupported"

// ErrUnsupportedVerb is the session-result error for a verb that *is* in
// SessionVerbs but this daemon does not implement (spec §3.2) — typically a
// daemon older than the verb. Distinct from ErrUnsupported: the verb is valid
// protocol, so an orchestrator may offer it again after the daemon upgrades, and
// should degrade its UI rather than treat the connection as broken.
const ErrUnsupportedVerb = "unsupported verb"

// Terminal registration errors (MuxMsg.Error on a rejected registered): the
// provider exits with the message instead of retrying.
const (
	ErrBadToken   = "bad-token"
	ErrRevoked    = "revoked"
	ErrBadVersion = "unsupported-version"
)

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
	Sessions []Session `json:"sessions,omitempty"` // sessions: full inventory snapshot (Seq is per-connection monotonic)
	ReqID    string    `json:"reqId,omitempty"`    // session-result: echoes the session-action reqId
	OK       bool      `json:"ok,omitempty"`       // session-result: verb succeeded
	NewID    string    `json:"newId,omitempty"`    // session-result: id of any session the verb created
	// Result is the disposition of a successful verb: ResultApplied (effect is
	// complete) or ResultAccepted (validated and taken on; the effect is
	// asynchronous). Empty means applied, so a daemon predating the field is read
	// correctly. Unset on a failure — Error carries that.
	Result string `json:"result,omitempty"` // session-result: applied | accepted
	// Accepted is the boolean form of Result == ResultAccepted, set on exactly the
	// same results. It is not decoration: an accepted verb may now be answered
	// *before* it is delivered — a `prompt` to a stopped agent acks and then cold-
	// starts the runtime — so "did this succeed?" and "is the work finished?" have
	// become genuinely different questions, and a consumer that must not block on
	// the second one should be able to ask the first without string-matching a
	// disposition vocabulary that may grow. Additive and omitempty: a consumer
	// predating it reads Result exactly as before.
	Accepted bool `json:"accepted,omitempty"` // session-result: work continues after this reply

	// v2 "runtime-events" feature (runtime-events frame). SessionID names the
	// published session; Seq is per-session monotonic (the ordinal of the last
	// event in Events); Events is a seq-ordered batch of structured transcript
	// events. A resuming consumer subscribes with afterSeq and receives only
	// events whose ordinal exceeds it. Runtime names the runtime whose record
	// produced the batch (Runtime* — "claude", "codex", …); it is additive and
	// omitted by a producer that predates it, so a consumer that needs a runtime
	// falls back to its own default when the field is absent.
	SessionID string         `json:"sessionId,omitempty"`
	Runtime   string         `json:"runtime,omitempty"`
	Events    []RuntimeEvent `json:"events,omitempty"`
}

// Conn is a typed harnessproto connection: one JSON object per line over any byte
// stream (unix socket, TCP under TLS, stdio, net.Pipe). Writes are serialized;
// reads return whole lines regardless of length, so large base64 pane payloads
// stream fine.
type Conn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	mu  sync.Mutex // serializes concurrent writers
}

// NewConn wraps a stream for harnessproto framing.
func NewConn(rwc io.ReadWriteCloser) *Conn {
	return &Conn{rwc: rwc, r: bufio.NewReaderSize(rwc, 64*1024)}
}

func (c *Conn) Close() error { return c.rwc.Close() }

func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.rwc.Write(b)
	return err
}

func (c *Conn) read(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Server-side helpers.
func (c *Conn) WriteMux(m MuxMsg) error { return c.write(m) }
func (c *Conn) ReadHarness() (HarnessMsg, error) {
	var m HarnessMsg
	err := c.read(&m)
	return m, err
}

// Harness-side helpers.
func (c *Conn) WriteHarness(m HarnessMsg) error { return c.write(m) }
func (c *Conn) ReadMux() (MuxMsg, error) {
	var m MuxMsg
	err := c.read(&m)
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
