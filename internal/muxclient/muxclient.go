// Package muxclient is the UI side of the muxproto protocol: it dials a local or
// remote multiplexer server and exposes its state stream and per-pane I/O. The
// same client drives a local server (unix socket) or any remote one (TCP), so a
// UI can attach to several servers at once.
package muxclient

import (
	"net"
	"strings"

	"amux/internal/core"
	"amux/internal/muxproto"
)

// Handlers receive asynchronous server events (called from the read goroutine).
type Handlers struct {
	OnSnapshot   func(sessions []core.Session)
	OnPaneOutput func(paneID string, data []byte)
	OnPaneExit   func(paneID string, errMsg string)
	OnResult     func(ok bool, errMsg string)
	OnClosed     func()
}

// Client is a connection to one multiplexer server.
type Client struct {
	conn *muxproto.Conn
	h    Handlers
}

// Dial connects to a server. spec: "" / "local" => the local unix socket;
// "unix:/path" => a unix socket; "host:port" or "tcp:host:port" => a remote TCP
// server.
func Dial(spec string, h Handlers) (*Client, error) {
	network, addr := resolve(spec)
	nc, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: muxproto.NewConn(nc), h: h}
	_ = c.conn.WriteClient(muxproto.ClientMsg{Type: muxproto.CHello, Version: muxproto.Version})
	go c.readLoop()
	return c, nil
}

func resolve(spec string) (network, addr string) {
	switch {
	case spec == "" || spec == "local":
		return "unix", core.MuxSocketPath()
	case strings.HasPrefix(spec, "unix:"):
		return "unix", strings.TrimPrefix(spec, "unix:")
	case strings.HasPrefix(spec, "tcp:"):
		return "tcp", strings.TrimPrefix(spec, "tcp:")
	default:
		return "tcp", spec
	}
}

func (c *Client) readLoop() {
	for {
		m, err := c.conn.ReadServer()
		if err != nil {
			if c.h.OnClosed != nil {
				c.h.OnClosed()
			}
			return
		}
		switch m.Type {
		case muxproto.SSnapshot:
			if c.h.OnSnapshot != nil {
				c.h.OnSnapshot(m.Sessions)
			}
		case muxproto.SPaneOutput:
			if c.h.OnPaneOutput != nil {
				c.h.OnPaneOutput(m.PaneID, m.Data)
			}
		case muxproto.SPaneExit:
			if c.h.OnPaneExit != nil {
				c.h.OnPaneExit(m.PaneID, m.Error)
			}
		case muxproto.SResult:
			if c.h.OnResult != nil {
				c.h.OnResult(m.OK, m.Error)
			}
		}
	}
}

func (c *Client) Close() error { return c.conn.Close() }

// Subscribe asks the server to stream rail snapshots.
func (c *Client) Subscribe() error {
	return c.conn.WriteClient(muxproto.ClientMsg{Type: muxproto.CSubscribe})
}

// Action runs a lifecycle action (create/move/archive/…).
func (c *Client) Action(a core.Action) error {
	return c.conn.WriteClient(muxproto.ClientMsg{
		Type: muxproto.CAction, Action: a.Action, ID: a.ID, Target: a.Target, Fields: a.Fields,
	})
}

// PaneOpen starts streaming a tab of an agent. The caller mints paneID.
func (c *Client) PaneOpen(paneID, agent string, tab, cols, rows int) error {
	return c.conn.WriteClient(muxproto.ClientMsg{
		Type: muxproto.CPaneOpen, PaneID: paneID, Agent: agent, Tab: tab, Cols: cols, Rows: rows,
	})
}

// PaneInput forwards keystrokes to a pane.
func (c *Client) PaneInput(paneID string, data []byte) error {
	return c.conn.WriteClient(muxproto.ClientMsg{Type: muxproto.CPaneInput, PaneID: paneID, Data: data})
}

// PaneResize forwards a resize to a pane.
func (c *Client) PaneResize(paneID string, cols, rows int) error {
	return c.conn.WriteClient(muxproto.ClientMsg{Type: muxproto.CPaneResize, PaneID: paneID, Cols: cols, Rows: rows})
}

// PaneClose stops a pane.
func (c *Client) PaneClose(paneID string) error {
	return c.conn.WriteClient(muxproto.ClientMsg{Type: muxproto.CPaneClose, PaneID: paneID})
}
