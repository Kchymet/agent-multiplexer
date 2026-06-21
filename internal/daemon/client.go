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

// Frame is a decoded inbound message: exactly one of Snapshot/Result is set.
type Frame struct {
	Snapshot *core.Snapshot
	Result   *core.Result
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
	default:
		// Unknown frame: return an empty frame so the caller can keep reading.
		return Frame{}, nil
	}
}
