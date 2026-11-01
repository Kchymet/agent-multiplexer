// Package hostbackend is the session-facing operation set of the local amux
// backend: the authenticated primary daemon that owns the store, the engine and
// every pane. It is the one seam through which transports reach those
// operations: the Harness relay provider (`amux provide`), the legacy
// compatibility mux (`amux serve`), and the native TUI's own connection.
//
// It deliberately knows nothing about how a remote peer reached amux. Provider
// transport, the relay wire and any future access route live with their own
// transports; this package only performs host-authorized daemon operations.
//
// Authority: every operation dials daemon.DialContext, which verifies the
// daemon's pinned TLS identity and proves the current host credential before any
// request is sent. One-shot operations use a fresh connection per call, so a
// rotated or revoked host credential takes effect at the next call and no
// caller can reuse an older authorization. Cancellation closes the
// authenticated transport and waits for the in-flight callback to observe the
// close before returning, so no callback outlives its caller.
package hostbackend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Conn is the authenticated daemon connection surface the operations use.
// *daemon.Client implements it; tests substitute fakes.
type Conn interface {
	Snapshot() ([]core.Session, error)
	Dispatch(core.Action) (string, error)
	RuntimeRecord(id string) (core.RuntimeRecord, error)
	Accounts(hp.AccountRequest, bool) (hp.AccountResult, error)
	PaneOpenContext(ctx context.Context, paneID, agentID string, tab, cols, rows int) error
	PaneInputContext(ctx context.Context, paneID string, data []byte) error
	PaneResizeContext(ctx context.Context, paneID string, cols, rows int) error
	NextContext(context.Context) (daemon.Frame, error)
	Close() error
}

// ErrUnavailable marks a call that never reached an authenticated daemon (no
// socket, failed TLS pin or host proof). Its text is the provider's existing
// "daemon unreachable: …" error, kept unchanged for Harness peers. The legacy
// mux previously prefixed the same failure "authenticate primary daemon: …";
// it now reports this text instead (cosmetic; no caller matches on it).
var ErrUnavailable = errors.New("daemon unreachable")

// PaneWriteTimeout bounds one pane input or resize write on a relay pane.
const PaneWriteTimeout = 5 * time.Second

// HostDaemon is the local amux backend. The zero value dials the real daemon.
type HostDaemon struct {
	dial func(context.Context) (Conn, error)
}

// New returns the production backend: the local primary daemon over its
// authenticated Unix socket.
func New() HostDaemon { return HostDaemon{} }

// WithDialer returns a backend whose connections come from dial. It exists for
// tests; production uses New.
func WithDialer(dial func(context.Context) (Conn, error)) HostDaemon {
	return HostDaemon{dial: dial}
}

func (h HostDaemon) open(ctx context.Context) (Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.dial != nil {
		return h.dial(ctx)
	}
	c, err := daemon.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Connect opens a long-lived authenticated session stream (snapshots, results
// and pane frames on one connection) for an interactive client such as the
// native TUI, with daemon.Dial's bounded connect-and-authenticate timeout. The
// caller owns and closes it.
func (HostDaemon) Connect() (*daemon.Client, error) { return daemon.Dial() }

// ConnectContext is Connect bounded by ctx instead of the fixed timeout.
func (HostDaemon) ConnectContext(ctx context.Context) (*daemon.Client, error) {
	return daemon.DialContext(ctx)
}

// Snapshot returns the daemon's current session inventory.
func (h HostDaemon) Snapshot(ctx context.Context) ([]core.Session, error) {
	return call(ctx, h, func(c Conn) ([]core.Session, error) { return c.Snapshot() })
}

// Apply runs one lifecycle/control action with the daemon as the authority and
// returns the id of any session it created. A cancelled call makes no claim that
// an effect the daemon already committed was rolled back.
func (h HostDaemon) Apply(ctx context.Context, a core.Action) (string, error) {
	return call(ctx, h, func(c Conn) (string, error) { return c.Dispatch(a) })
}

// RuntimeRecord resolves a session to its runtime transcript record.
func (h HostDaemon) RuntimeRecord(ctx context.Context, sessionID string) (core.RuntimeRecord, error) {
	return call(ctx, h, func(c Conn) (core.RuntimeRecord, error) { return c.RuntimeRecord(sessionID) })
}

// call gives one operation a fresh authenticated connection, closed on return.
func call[T any](ctx context.Context, h HostDaemon, op func(Conn) (T, error)) (T, error) {
	var zero T
	c, err := h.open(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return zero, ctxErr
		}
		return zero, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer c.Close()
	return Join(ctx, func() { _ = c.Close() }, func() (T, error) { return op(c) })
}

type result[T any] struct {
	value T
	err   error
}

// Join runs call and owns its lifetime. Cancellation first closes the
// transport, then waits for call to observe that close; the late value is
// discarded and ctx.Err is returned. The buffered result means the worker never
// blocks on delivery.
func Join[T any](ctx context.Context, closeTransport func(), call func() (T, error)) (T, error) {
	var zero T
	done := make(chan result[T], 1)
	go func() {
		value, err := call()
		done <- result[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		closeTransport()
		<-done
		return zero, ctx.Err()
	case r := <-done:
		return r.value, r.err
	}
}

// Accounts is one dedicated authenticated account connection. A ceremony
// belongs to it, never to a pane or event stream; closing it retires every
// pending attempt, including a request whose reply was lost.
type Accounts struct {
	c    Conn
	stop func() bool
}

// OpenAccounts opens an account connection whose lifetime is bounded by ctx:
// cancellation closes it.
func (h HostDaemon) OpenAccounts(ctx context.Context) (*Accounts, error) {
	c, err := h.open(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	return &Accounts{c: c, stop: stop}, nil
}

// Call performs one account request on this connection.
func (a *Accounts) Call(ctx context.Context, r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
	if err := ctx.Err(); err != nil {
		return hp.AccountResult{}, err
	}
	return a.c.Accounts(r, ceremony)
}

// Close closes the account connection.
func (a *Accounts) Close() error {
	a.stop()
	return a.c.Close()
}

// PaneRequest is the only pane authority a relay may pass to the daemon.
type PaneRequest struct {
	Agent      string
	Tab        int
	Cols, Rows int
}

// Pane is one daemon-owned pane stream on its own authenticated connection.
// Closing it detaches this subscriber; the daemon, not the relay, decides the
// agent runtime's lifetime.
type Pane struct {
	c      Conn
	paneID string
	once   sync.Once
	err    error
}

const relayPaneID = "relay-pane"

// OpenPane authenticates a fresh connection and attaches it to one pane.
func (h HostDaemon) OpenPane(ctx context.Context, r PaneRequest) (*Pane, error) {
	c, err := h.open(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("authenticate primary daemon pane relay: %w", err)
	}
	p := &Pane{c: c, paneID: relayPaneID}
	if err := c.PaneOpenContext(ctx, p.paneID, r.Agent, r.Tab, r.Cols, r.Rows); err != nil {
		_ = c.Close()
		return nil, err
	}
	return p, nil
}

// Next returns the next frame of this pane, skipping unrelated frames on the
// connection (snapshots, results, data).
func (p *Pane) Next(ctx context.Context) (core.PaneFrame, error) {
	for {
		frame, err := p.c.NextContext(ctx)
		if err != nil {
			return core.PaneFrame{}, err
		}
		if frame.Pane != nil && frame.Pane.PaneID == p.paneID {
			return *frame.Pane, nil
		}
	}
}

// Input forwards bytes to the pane within PaneWriteTimeout.
func (p *Pane) Input(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), PaneWriteTimeout)
	defer cancel()
	return p.c.PaneInputContext(ctx, p.paneID, data)
}

// Resize forwards a resize within PaneWriteTimeout.
func (p *Pane) Resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), PaneWriteTimeout)
	defer cancel()
	return p.c.PaneResizeContext(ctx, p.paneID, cols, rows)
}

// Close is the authoritative, idempotent detach: closing the connection
// interrupts every blocked read and write, with no queued best-effort frame
// delaying the revocation barrier.
func (p *Pane) Close() error {
	p.once.Do(func() {
		p.err = p.c.Close()
		if errors.Is(p.err, net.ErrClosed) {
			p.err = nil
		}
	})
	return p.err
}
