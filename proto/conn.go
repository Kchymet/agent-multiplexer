package proto

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"io"
	"sync"
)

// Conn frames protocol messages over any byte stream (TCP, unix socket, stdio,
// TLS): one JSON object per line. Writes are serialized so concurrent senders are
// safe; reads return whole lines regardless of length, so large base64 pane
// payloads stream fine. It is the self-contained public mirror of amux's internal
// transport, so a consumer needs nothing beyond this module to speak the protocol.
type Conn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	mu  sync.Mutex // serializes concurrent writers
}

// NewConn wraps a stream as a line-framed protocol connection.
func NewConn(rwc io.ReadWriteCloser) *Conn {
	return &Conn{rwc: rwc, r: bufio.NewReaderSize(rwc, 64*1024)}
}

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rwc.Close() }

// write marshals v and appends a newline. Safe for concurrent callers.
func (c *Conn) write(v any) error {
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

// read decodes the next line into v.
func (c *Conn) read(v any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Orchestrator-side helpers.

// WriteMux sends an orchestrator -> provider message.
func (c *Conn) WriteMux(m MuxMsg) error { return c.write(m) }

// ReadHarness reads the next provider -> orchestrator message.
func (c *Conn) ReadHarness() (HarnessMsg, error) {
	var m HarnessMsg
	err := c.read(&m)
	return m, err
}

// Provider-side helpers.

// WriteHarness sends a provider -> orchestrator message.
func (c *Conn) WriteHarness(m HarnessMsg) error { return c.write(m) }

// ReadMux reads the next orchestrator -> provider message.
func (c *Conn) ReadMux() (MuxMsg, error) {
	var m MuxMsg
	err := c.read(&m)
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
