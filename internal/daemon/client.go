package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"sync"
	"time"

	"amux/internal/core"
)

// Client is a connection to the daemon. It decodes the inbound frame stream
// (snapshots and action results) one message at a time via Next, and sends
// actions via Send.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex // serializes writes
}

// Dial connects to the daemon socket (single attempt).
func Dial() (*Client, error) {
	conn, err := net.DialTimeout("unix", core.SocketPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, r: bufio.NewReader(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Send writes an action to the daemon.
func (c *Client) Send(a core.Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.conn.Write(b)
	return err
}

// PaneOpen asks the daemon to attach this connection to a tab of an agent,
// streaming its output back as pane frames. The caller mints paneID (unique
// within the connection).
func (c *Client) PaneOpen(paneID, agentID string, tab, cols, rows int) error {
	return c.Send(core.Action{Action: core.ActionPaneOpen, PaneID: paneID, ID: agentID, Tab: tab, Cols: cols, Rows: rows})
}

// PaneInput forwards input bytes to an attached pane.
func (c *Client) PaneInput(paneID string, data []byte) error {
	return c.Send(core.Action{Action: core.ActionPaneInput, PaneID: paneID, Data: data})
}

// PaneResize forwards a resize to an attached pane.
func (c *Client) PaneResize(paneID string, cols, rows int) error {
	return c.Send(core.Action{Action: core.ActionPaneResize, PaneID: paneID, Cols: cols, Rows: rows})
}

// PaneClose detaches a pane (the agent keeps running in the daemon's engine).
func (c *Client) PaneClose(paneID string) error {
	return c.Send(core.Action{Action: core.ActionPaneClose, PaneID: paneID})
}

// Frame is a decoded inbound message: exactly one of Snapshot/Result/Pane is set.
type Frame struct {
	Snapshot *core.Snapshot
	Result   *core.Result
	Pane     *core.PaneFrame
}

// Next blocks until the next frame arrives (or the connection errors).
func (c *Client) Next() (Frame, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return Frame{}, err
	}
	var env struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return Frame{}, err
	}
	switch env.Type {
	case "snapshot":
		var s core.Snapshot
		if err := json.Unmarshal(line, &s); err != nil {
			return Frame{}, err
		}
		return Frame{Snapshot: &s}, nil
	case "result":
		var r core.Result
		if err := json.Unmarshal(line, &r); err != nil {
			return Frame{}, err
		}
		return Frame{Result: &r}, nil
	case core.FramePaneOutput, core.FramePaneExit:
		var p core.PaneFrame
		if err := json.Unmarshal(line, &p); err != nil {
			return Frame{}, err
		}
		return Frame{Pane: &p}, nil
	default:
		// Unknown frame: return an empty frame so the caller can keep reading.
		return Frame{}, nil
	}
}
