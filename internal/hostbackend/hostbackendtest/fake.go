// Package hostbackendtest provides a scripted fake daemon connection for tests
// of hostbackend and the transports routed through it.
package hostbackendtest

import (
	"context"
	"net"
	"sync"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Conn is a fake authenticated daemon connection. Hooks left nil return zero
// values. Every call is recorded in Calls; Close is idempotent and unblocks
// Block-based hooks via Closed.
type Conn struct {
	Label string

	SnapshotFn      func() ([]core.Session, error)
	DispatchFn      func(core.Action) (string, error)
	RuntimeRecordFn func(string) (core.RuntimeRecord, error)
	AccountsFn      func(hp.AccountRequest, bool) (hp.AccountResult, error)
	Frames          chan daemon.Frame

	mu     sync.Mutex
	calls  []string
	closed chan struct{}
	once   sync.Once
}

// New returns a fake connection with a buffered frame queue.
func New(label string) *Conn {
	return &Conn{Label: label, Frames: make(chan daemon.Frame, 16), closed: make(chan struct{})}
}

func (c *Conn) record(call string) {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
}

// Calls returns the recorded operation names in order.
func (c *Conn) Calls() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// Closed is closed once Close has been called.
func (c *Conn) Closed() <-chan struct{} { return c.closed }

// IsClosed reports whether Close has been called.
func (c *Conn) IsClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Conn) Snapshot() ([]core.Session, error) {
	c.record("snapshot")
	if c.SnapshotFn != nil {
		return c.SnapshotFn()
	}
	return nil, nil
}

func (c *Conn) Dispatch(a core.Action) (string, error) {
	c.record("dispatch:" + a.Action)
	if c.DispatchFn != nil {
		return c.DispatchFn(a)
	}
	return "", nil
}

func (c *Conn) RuntimeRecord(id string) (core.RuntimeRecord, error) {
	c.record("runtime-record:" + id)
	if c.RuntimeRecordFn != nil {
		return c.RuntimeRecordFn(id)
	}
	return core.RuntimeRecord{}, nil
}

func (c *Conn) Accounts(r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
	c.record("accounts:" + r.Operation)
	if c.AccountsFn != nil {
		return c.AccountsFn(r, ceremony)
	}
	return hp.AccountResult{}, nil
}

func (c *Conn) PaneOpenContext(_ context.Context, paneID, agentID string, tab, cols, rows int) error {
	c.record("pane-open:" + paneID + ":" + agentID)
	return c.err()
}

func (c *Conn) PaneInputContext(_ context.Context, paneID string, data []byte) error {
	c.record("pane-input:" + paneID + ":" + string(data))
	return c.err()
}

func (c *Conn) PaneResizeContext(_ context.Context, paneID string, cols, rows int) error {
	c.record("pane-resize:" + paneID)
	return c.err()
}

func (c *Conn) NextContext(ctx context.Context) (daemon.Frame, error) {
	select {
	case f := <-c.Frames:
		return f, nil
	case <-c.closed:
		return daemon.Frame{}, net.ErrClosed
	case <-ctx.Done():
		return daemon.Frame{}, ctx.Err()
	}
}

func (c *Conn) err() error {
	if c.IsClosed() {
		return net.ErrClosed
	}
	return nil
}

func (c *Conn) Close() error {
	c.record("close")
	err := error(nil)
	if c.IsClosed() {
		err = net.ErrClosed
	}
	c.once.Do(func() { close(c.closed) })
	return err
}

// Dialer hands out connections in order and records each dial. When the queue
// is exhausted or Fail is set, it returns Fail (or an error) without a
// connection — the shape of a revoked or rotated-away host credential.
type Dialer struct {
	mu    sync.Mutex
	queue []*Conn
	Fail  error
	dials int
}

// NewDialer queues conns for successive dials.
func NewDialer(conns ...*Conn) *Dialer { return &Dialer{queue: conns} }

// Dials reports how many dials were attempted.
func (d *Dialer) Dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// SetFail makes every subsequent dial fail with err.
func (d *Dialer) SetFail(err error) {
	d.mu.Lock()
	d.Fail = err
	d.mu.Unlock()
}

// Dial implements the hostbackend dialer.
func (d *Dialer) Dial(context.Context) (hostbackend.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.Fail != nil {
		return nil, d.Fail
	}
	if len(d.queue) == 0 {
		return nil, errNoConn
	}
	c := d.queue[0]
	d.queue = d.queue[1:]
	return c, nil
}

// Backend returns a HostDaemon whose connections come from d.
func (d *Dialer) Backend() hostbackend.HostDaemon { return hostbackend.WithDialer(d.Dial) }

type dialErr string

func (e dialErr) Error() string { return string(e) }

const errNoConn = dialErr("host credential rejected")
