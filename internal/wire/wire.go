// Package wire is the shared transport for amux's client/server protocols: one
// JSON object per line over any byte stream (unix socket, TCP, stdio, net.Pipe).
// Writes are serialized; reads accept bounded whole lines large enough for the
// protocol's base64 pane payloads.
package wire

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// MaxFrameBytes bounds one JSON line before decoding. Pane output is capped at
// four MiB before base64 encoding, so eight MiB preserves valid frames while a
// newline-free or oversized peer cannot grow memory without limit.
const MaxFrameBytes = 8 << 20

var ErrFrameTooLarge = errors.New("wire: frame exceeds 8 MiB")

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
	line := make([]byte, 0, 64*1024)
	for {
		fragment, err := c.r.ReadSlice('\n')
		if len(line)+len(fragment) > MaxFrameBytes {
			return ErrFrameTooLarge
		}
		line = append(line, fragment...)
		switch err {
		case nil:
			return json.Unmarshal(line, v)
		case bufio.ErrBufferFull:
			continue
		default:
			return err
		}
	}
}

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rwc.Close() }
