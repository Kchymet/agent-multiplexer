// Package harnessproto is the Multiplexer Server <-> Agent Harness wire protocol:
// the server tells a harness to spawn/feed/kill PTY-backed panes, and the harness
// streams their output back. See docs/client-server.md. One JSON object per line;
// pane bytes ride in Data ([]byte, base64-encoded by encoding/json).
package harnessproto

import (
	"crypto/subtle"
	"io"

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
)

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
