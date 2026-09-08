package mux

import (
	"context"
	"fmt"
	"net"
	"sync"

	"amux/internal/core"
	"amux/internal/daemon"
)

// daemonPrimary is a deliberately short-lived relay to the singleton daemon.
// A fresh Dial performs pinned TLS server authentication and proves the current
// host credential for every mux operation, which also makes rotation/revocation
// an admission boundary. Keeping queries separate avoids trying to correlate
// results on the daemon's asynchronous snapshot stream.
type daemonPrimary struct{}

func (daemonPrimary) Snapshot(ctx context.Context) ([]core.Session, error) {
	return daemonCall(ctx, func(c *daemon.Client) ([]core.Session, error) { return c.Snapshot() })
}

func (daemonPrimary) Dispatch(ctx context.Context, action core.Action) (string, error) {
	return daemonCall(ctx, func(c *daemon.Client) (string, error) { return c.Dispatch(action) })
}

func (daemonPrimary) OpenPane(ctx context.Context, request PaneRequest) (PaneRelay, error) {
	c, err := daemon.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("authenticate primary daemon pane relay: %w", err)
	}
	r := &daemonPaneRelay{client: c, paneID: "legacy-pane"}
	if err := c.PaneOpenContext(ctx, r.paneID, request.Agent, request.Tab, request.Cols, request.Rows); err != nil {
		_ = c.Close()
		return nil, err
	}
	return r, nil
}

type daemonPaneRelay struct {
	client *daemon.Client
	paneID string
	once   sync.Once
}

func (r *daemonPaneRelay) Next(ctx context.Context) (core.PaneFrame, error) {
	for {
		frame, err := r.client.NextContext(ctx)
		if err != nil {
			return core.PaneFrame{}, err
		}
		if frame.Pane != nil && frame.Pane.PaneID == r.paneID {
			return *frame.Pane, nil
		}
		// Snapshot/result/data frames are unrelated to this one-purpose stream.
	}
}

func (r *daemonPaneRelay) Input(data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), primaryReadTimeout)
	defer cancel()
	return r.client.PaneInputContext(ctx, r.paneID, data)
}

func (r *daemonPaneRelay) Resize(cols, rows int) error {
	ctx, cancel := context.WithTimeout(context.Background(), primaryReadTimeout)
	defer cancel()
	return r.client.PaneResizeContext(ctx, r.paneID, cols, rows)
}

func (r *daemonPaneRelay) Close() error {
	var err error
	r.once.Do(func() {
		// Connection close is the authoritative detach. It interrupts every
		// blocked read/write immediately; no queued best-effort close frame is
		// allowed to delay the revocation barrier.
		err = r.client.Close()
	})
	if err == nil {
		return nil
	}
	if err == net.ErrClosed {
		return nil
	}
	return err
}

type daemonResult[T any] struct {
	value T
	err   error
}

// daemonCall gives every relay operation a fresh authenticated connection and
// closes it on cancellation. The buffered result lets the worker exit even if
// cancellation wins the select; no goroutine remains queued on delivery.
func daemonCall[T any](ctx context.Context, call func(*daemon.Client) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c, err := daemon.DialContext(ctx)
	if err != nil {
		return zero, fmt.Errorf("authenticate primary daemon: %w", err)
	}
	defer c.Close()
	done := make(chan daemonResult[T], 1)
	go func() {
		value, err := call(c)
		done <- daemonResult[T]{value: value, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = c.Close()
		return zero, ctx.Err()
	case result := <-done:
		return result.value, result.err
	}
}
