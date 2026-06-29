// Package wire is the shared transport for amux's client/server protocols: one
// JSON object per line over any byte stream (unix socket, TCP, stdio, net.Pipe).
// Writes are serialized; reads return whole lines regardless of length, so large
// base64 pane payloads stream fine.
package wire

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// Conn frames JSON messages over a byte stream.
type Conn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	mu  sync.Mutex // serializes concurrent writers
}

// New wraps a stream as a line-framed JSON connection.
func New(rwc io.ReadWriteCloser) *Conn {
	return &Conn{rwc: rwc, r: bufio.NewReaderSize(rwc, 64*1024)}
}

// Write marshals v and appends a newline. Safe for concurrent callers.
func (c *Conn) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.rwc.Write(b)
	return err
}

// Read decodes the next line into v.
func (c *Conn) Read(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rwc.Close() }
