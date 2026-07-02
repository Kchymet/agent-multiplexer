// Package harnessproto is the Multiplexer Server <-> Agent Harness wire protocol:
// the server tells a harness to spawn/feed/kill PTY-backed panes, and the harness
// streams their output back. See docs/client-server.md. One JSON object per line;
// pane bytes ride in Data ([]byte, base64-encoded by encoding/json).
//
// The wire surface now lives in the public, importable module
// github.com/kchymet/agent-multiplexer/proto (docs/remote-provider.md). This
// package is a thin re-export of that single definition so amux's internal
// callers keep their short import path while external orchestrators import proto
// directly instead of hand-mirroring the message types.
package harnessproto

import "github.com/kchymet/agent-multiplexer/proto"

// Protocol versions (see proto).
const (
	Version  = proto.Version
	Version2 = proto.Version2
)

// Server -> harness (v2: orchestrator -> provider) message types.
const (
	MHello                  = proto.MHello
	MSpawn                  = proto.MSpawn
	MInput                  = proto.MInput
	MResize                 = proto.MResize
	MKill                   = proto.MKill
	MRegistered             = proto.MRegistered
	MPong                   = proto.MPong
	MSessionsSubscribe      = proto.MSessionsSubscribe
	MSessionAction          = proto.MSessionAction
	MRuntimeEventsSubscribe = proto.MRuntimeEventsSubscribe
)

// Harness -> server (v2: provider -> orchestrator) message types.
const (
	HReady         = proto.HReady
	HOutput        = proto.HOutput
	HExit          = proto.HExit
	HRegister      = proto.HRegister
	HReset         = proto.HReset
	HPing          = proto.HPing
	HSessions      = proto.HSessions
	HSessionResult = proto.HSessionResult
	HRuntimeEvents = proto.HRuntimeEvents
)

// Opt-in feature strings.
const (
	SessionsFeature      = proto.SessionsFeature
	RuntimeEventsFeature = proto.RuntimeEventsFeature
)

// Session lifecycle verbs.
const (
	VerbNewWorkgroup = proto.VerbNewWorkgroup
	VerbAddAgent     = proto.VerbAddAgent
	VerbRename       = proto.VerbRename
	VerbArchive      = proto.VerbArchive
	VerbUnarchive    = proto.VerbUnarchive
	VerbStart        = proto.VerbStart
)

// Protocol errors.
const (
	ErrUnsupported = proto.ErrUnsupported
	ErrBadToken    = proto.ErrBadToken
	ErrRevoked     = proto.ErrRevoked
	ErrBadVersion  = proto.ErrBadVersion
)

// Wire types — aliases to the single canonical definitions in proto.
type (
	RuntimeEvent      = proto.RuntimeEvent
	RuntimeEventBatch = proto.RuntimeEventBatch
	Capabilities      = proto.Capabilities
	PaneOffer         = proto.PaneOffer
	AdoptPane         = proto.AdoptPane
	MuxMsg            = proto.MuxMsg
	HarnessMsg        = proto.HarnessMsg
	Conn              = proto.Conn
)

// Codec + helpers re-exported from proto.
var (
	NewConn   = proto.NewConn
	TokenOK   = proto.TokenOK
	Negotiate = proto.Negotiate
)
