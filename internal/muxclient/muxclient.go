// Package muxclient is the UI side of the muxproto protocol: it dials a local or
// remote multiplexer server and exposes its state stream and per-pane I/O. The
// same client drives a local server (unix socket) or any remote one (TCP), so a
// UI can attach to several servers at once.
package muxclient

import (
	"fmt"
	"net"
	"os"
	"strings"

	"amux/internal/core"
	"amux/internal/muxproto"
	"amux/internal/wiretls"
)

// Handlers receive asynchronous server events (called from the read goroutine).
type Handlers struct {
	OnSnapshot   func(sessions []core.Session)
	OnPaneOutput func(paneID string, data []byte)
	// OnPaneReset fires when the server fell too far behind to stream losslessly
	// and is about to replay a fresh repaint: the client must clear its emulator
	// for paneID before applying subsequent output, so stale cells don't ghost.
	OnPaneReset func(paneID string)
	OnPaneExit  func(paneID string, errMsg string)
	OnResult    func(ok bool, errMsg string)
	OnClosed    func()
}

// Client is a connection to one multiplexer server.
type Client struct {
	conn   *muxproto.Conn
	h      Handlers
	server string // server identity from the welcome
}

// Server returns the server's advertised identity (from the welcome frame).
func (c *Client) Server() string { return c.server }

// Dial connects to a server. spec: "" / "local" => the local unix socket;
// "unix:/path" => a unix socket; "tcp:host:port" or bare "host:port" => plain
// TCP; "tls:host:port" (or "tls://host:port") => TLS over TCP, verifying the
// server per wiretls (system roots plus $AMUX_TLS_CA). When $AMUX_MUX_TOKEN is
// set it is sent as the hello bearer token.
func Dial(spec string, h Handlers) (*Client, error) {
	nc, err := dialConn(spec)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: muxproto.NewConn(nc), h: h}
	if err := c.conn.WriteClient(muxproto.ClientMsg{
		Type: muxproto.CHello, Version: muxproto.Version, Token: os.Getenv("AMUX_MUX_TOKEN"),
	}); err != nil {
		_ = nc.Close()
		return nil, err
	}
	// The welcome is always the first frame; read it synchronously so a terminal
	// rejection (bad token / unsupported version) surfaces as a Dial error rather
	// than a silent close.
	w, err := c.conn.ReadServer()
	if err != nil {
		_ = nc.Close()
		return nil, err
	}
	if w.Type != muxproto.SWelcome || w.Error != "" {
		_ = nc.Close()
		reason := w.Error
		if reason == "" {
			reason = "no welcome from server"
		}
		return nil, fmt.Errorf("mux: connection rejected: %s", reason)
	}
	c.server = w.Server
	go c.readLoop()
	return c, nil
}

func dialConn(spec string) (net.Conn, error) {
	network, addr := resolve(spec)
	if network == "tls" {
		return wiretls.Dial("tcp", addr)
	}
	return net.Dial(network, addr)
}

func resolve(spec string) (network, addr string) {
	switch {
	case spec == "" || spec == "local":
		return "unix", core.MuxSocketPath()
	case strings.HasPrefix(spec, "unix:"):
		return "unix", strings.TrimPrefix(spec, "unix:")
	case strings.HasPrefix(spec, "tls:"):
		return "tls", strings.TrimPrefix(strings.TrimPrefix(spec, "tls:"), "//")
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
		case muxproto.SPaneReset:
			if c.h.OnPaneReset != nil {
				c.h.OnPaneReset(m.PaneID)
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
