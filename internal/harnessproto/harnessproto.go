// Package harnessproto is the Multiplexer Server <-> Agent Harness wire protocol:
// the server tells a harness to spawn/feed/kill PTY-backed panes, and the harness
// streams their output back. See docs/client-server.md. One JSON object per line;
// pane bytes ride in Data ([]byte, base64-encoded by encoding/json).
package harnessproto

import (
	"io"

	"amux/internal/wire"
)

// Version is the protocol version exchanged in hello/ready.
const Version = 1

// Server -> harness message types.
const (
	MHello  = "hello"
	MSpawn  = "spawn"
	MInput  = "input"
	MResize = "resize"
	MKill   = "kill"
)

// Harness -> server message types.
const (
	HReady  = "ready"
	HOutput = "output"
	HExit   = "exit"
)

// MuxMsg is a server -> harness message.
type MuxMsg struct {
	Type    string   `json:"type"`
	Version int      `json:"version,omitempty"` // hello
	PaneID  string   `json:"paneId,omitempty"`
	Dir     string   `json:"dir,omitempty"`  // spawn: working directory
	Env     []string `json:"env,omitempty"`  // spawn: KEY=VALUE additions
	Argv    []string `json:"argv,omitempty"` // spawn: command + args
	Cols    int      `json:"cols,omitempty"` // spawn/resize
	Rows    int      `json:"rows,omitempty"` // spawn/resize
	Data    []byte   `json:"data,omitempty"` // input bytes
}

// HarnessMsg is a harness -> server message.
type HarnessMsg struct {
	Type    string `json:"type"`
	Version int    `json:"version,omitempty"` // ready
	PaneID  string `json:"paneId,omitempty"`
	Data    []byte `json:"data,omitempty"`  // output bytes
	Error   string `json:"error,omitempty"` // exit
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
