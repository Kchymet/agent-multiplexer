package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"amux/internal/core"
)

// outBuf bounds the client's outbound queue. Actions and pane input/responses
// are small; this is deep enough that it only ever fills if the socket itself is
// wedged (which the daemon's non-blocking serve loop is designed to prevent).
const outBuf = 1024

// Client is a connection to the daemon. It decodes the inbound frame stream
// (snapshots and action results) one message at a time via Next, and sends
// actions via Send. Send never touches the socket directly: it hands the encoded
// frame to a dedicated writer goroutine over a buffered channel, so a stalled
// socket write can't block the caller (the Bubble Tea Update goroutine, or the
// vterm response-drain goroutine that forwards the emulator's query replies).
type Client struct {
	conn net.Conn
	r    *bufio.Reader

	out  chan []byte
	done chan struct{}
	once sync.Once
}

// Dial connects to the daemon socket (single attempt).
func Dial() (*Client, error) {
	conn, err := net.DialTimeout("unix", core.SocketPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	return newClient(conn), nil
}

// newClient wraps a connection and starts its writer goroutine. Used by Dial and
// by tests that dial over an in-memory pipe.
func newClient(conn net.Conn) *Client {
	c := &Client{
		conn: conn,
		r:    bufio.NewReader(conn),
		out:  make(chan []byte, outBuf),
		done: make(chan struct{}),
	}
	go c.writeLoop()
	return c
}

// writeLoop is the single writer for the connection: it drains the outbound
// queue to the socket in FIFO order, so byte ordering of input (and query
// replies) is preserved. A write error stops the loop; the reader side surfaces
// the broken connection as an error from Next, which the UI turns into a
// reconnect.
func (c *Client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			if _, err := c.conn.Write(b); err != nil {
				c.stop()
				return
			}
		}
	}
}

func (c *Client) stop() { c.once.Do(func() { close(c.done) }) }

// Close stops the writer and closes the connection.
func (c *Client) Close() error {
	c.stop()
	return c.conn.Close()
}

// Send enqueues an action for the writer goroutine without blocking the caller.
// It returns an error only if the action can't be marshalled or the connection
// is already gone; if the outbound queue is full (a wedged socket) the frame is
// dropped rather than stalling the UI, and the ensuing read error triggers a
// reconnect. Ordering is preserved because every frame goes through the same
// FIFO channel and single writer.
func (c *Client) Send(a core.Action) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	select {
	case c.out <- b:
		return nil
	case <-c.done:
		return net.ErrClosed
	default:
		return net.ErrClosed
	}
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

// Query asks the daemon for a store-backed read model (QueryRepos, QuerySessions)
// and returns its rows as raw JSON for the caller to unmarshal into the matching
// row type. Snapshot frames that arrive first are skipped; a failed Data frame
// surfaces the daemon's error. This is the read counterpart to Send: it keeps the
// CLI from opening the store itself.
func (c *Client) Query(name string) (json.RawMessage, error) {
	if err := c.Send(core.Action{Action: core.ActionQuery, Query: name}); err != nil {
		return nil, err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return nil, err
		}
		if f.Data == nil {
			continue
		}
		if !f.Data.OK {
			return nil, fmt.Errorf("%s", f.Data.Error)
		}
		return f.Data.Rows, nil
	}
}

// Frame is a decoded inbound message: exactly one of Snapshot/Result/Pane/Data
// is set.
type Frame struct {
	Snapshot *core.Snapshot
	Result   *core.Result
	Pane     *core.PaneFrame
	Data     *core.Data
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
	case core.FrameData:
		var dm core.Data
		if err := json.Unmarshal(line, &dm); err != nil {
			return Frame{}, err
		}
		return Frame{Data: &dm}, nil
	default:
		// Unknown frame: return an empty frame so the caller can keep reading.
		return Frame{}, nil
	}
}
