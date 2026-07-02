// Package proto is the public, importable wire protocol for agent-multiplexer's
// remote-provider link: the orchestrator tells a provider to spawn/feed/kill
// PTY-backed panes, the provider streams their output back, and — via opt-in
// features — publishes its session inventory and structured transcript events.
//
// It is the canonical home of every message type on that link. External
// orchestrators should import it at
//
//	github.com/Kchymet/agent-multiplexer/proto
//
// instead of hand-mirroring the message shapes. The framing is one JSON object
// per line (see [Conn]); pane bytes ride in Data ([]byte, base64-encoded by
// encoding/json). See docs/remote-provider.md and docs/remote-provider-sessions.md.
//
// The module carries no dependencies beyond the Go standard library, so it stays
// cheap to import. Wire compatibility is locked by a golden JSON-keys test
// (proto_test.go); the field tags here are the contract.
package proto

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

// Orchestrator -> provider message types (the "mux"/server role). In the v1
// in-process handshake these travel server -> harness.
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

// Provider -> orchestrator message types (the "harness" role). In the v1
// in-process handshake these travel harness -> server.
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
