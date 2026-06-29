// Package muxproto is the UI <-> Multiplexer Server wire protocol: the messages a
// UI client and a multiplexer server exchange over a wire.Conn. See
// docs/client-server.md. One JSON object per line; pane bytes ride in Data
// ([]byte, base64-encoded by encoding/json).
package muxproto

import (
	"io"

	"amux/internal/core"
	"amux/internal/wire"
)

// Version is the protocol version exchanged in hello/welcome.
const Version = 1

// Pane tabs (mirror the UI's per-agent tabs).
const (
	TabAgent    = 0
	TabEditor   = 1
	TabTerminal = 2
)

// Client message types.
const (
	CHello      = "hello"
	CSubscribe  = "subscribe"
	CAction     = "action"
	CPaneOpen   = "pane.open"
	CPaneInput  = "pane.input"
	CPaneResize = "pane.resize"
	CPaneClose  = "pane.close"
)

// Server message types.
const (
	SWelcome    = "welcome"
	SSnapshot   = "snapshot"
	SResult     = "result"
	SPaneOutput = "pane.output"
	SPaneExit   = "pane.exit"
)

// ClientMsg is a UI -> server message. Type selects which fields apply.
type ClientMsg struct {
	Type    string            `json:"type"`
	Version int               `json:"version,omitempty"` // hello
	Action  string            `json:"action,omitempty"`  // action: lifecycle verb (mirrors core.Action)
	ID      string            `json:"id,omitempty"`      // action / pane target id
	Target  string            `json:"target,omitempty"`  // action: move destination
	Fields  map[string]string `json:"fields,omitempty"`  // action: form fields
	PaneID  string            `json:"paneId,omitempty"`  // pane.*: client-minted stream id
	Agent   string            `json:"agent,omitempty"`   // pane.open: agent id
	Tab     int               `json:"tab,omitempty"`     // pane.open: TabAgent|TabEditor|TabTerminal
	Cols    int               `json:"cols,omitempty"`    // pane.open/resize
	Rows    int               `json:"rows,omitempty"`    // pane.open/resize
	Data    []byte            `json:"data,omitempty"`    // pane.input bytes
}

// ServerMsg is a server -> UI message.
type ServerMsg struct {
	Type     string         `json:"type"`
	Version  int            `json:"version,omitempty"`  // welcome
	Server   string         `json:"server,omitempty"`   // welcome: server identity
	Sessions []core.Session `json:"sessions,omitempty"` // snapshot
	OK       bool           `json:"ok,omitempty"`       // result
	Error    string         `json:"error,omitempty"`    // result / pane.exit
	PaneID   string         `json:"paneId,omitempty"`   // pane.output / pane.exit
	Data     []byte         `json:"data,omitempty"`     // pane.output bytes
}

// Conn is a typed muxproto connection.
type Conn struct{ w *wire.Conn }

// NewConn wraps a stream for muxproto.
func NewConn(rwc io.ReadWriteCloser) *Conn { return &Conn{w: wire.New(rwc)} }

func (c *Conn) Close() error { return c.w.Close() }

// Client-side helpers.
func (c *Conn) WriteClient(m ClientMsg) error { return c.w.Write(m) }
func (c *Conn) ReadServer() (ServerMsg, error) {
	var m ServerMsg
	err := c.w.Read(&m)
	return m, err
}

// Server-side helpers.
func (c *Conn) WriteServer(m ServerMsg) error { return c.w.Write(m) }
func (c *Conn) ReadClient() (ClientMsg, error) {
	var m ClientMsg
	err := c.w.Read(&m)
	return m, err
}
