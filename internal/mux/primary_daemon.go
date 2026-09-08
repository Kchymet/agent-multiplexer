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
	if err := c.PaneOpen(r.paneID, request.Agent, request.Tab, request.Cols, request.Rows); err != nil {
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
	type result struct {
		frame daemon.Frame
		err   error
	}
	for {
		done := make(chan result, 1)
		go func() {
			frame, err := r.client.Next()
			done <- result{frame: frame, err: err}
		}()
		select {
		case <-ctx.Done():
			_ = r.Close() // closes the socket and interrupts the blocked read
			<-done        // join the read before publishing route completion
			return core.PaneFrame{}, ctx.Err()
		case got := <-done:
			if got.err != nil {
				return core.PaneFrame{}, got.err
			}
			if got.frame.Pane != nil && got.frame.Pane.PaneID == r.paneID {
				return *got.frame.Pane, nil
			}
			// Snapshot/result/data frames are unrelated to this one-purpose stream.
		}
	}
}

func (r *daemonPaneRelay) Input(data []byte) error {
	return r.client.PaneInput(r.paneID, data)
}

func (r *daemonPaneRelay) Resize(cols, rows int) error {
	return r.client.PaneResize(r.paneID, cols, rows)
}

func (r *daemonPaneRelay) Close() error {
	var err error
	r.once.Do(func() {
		// Connection close is the authoritative detach and interrupts blocked I/O;
		// PaneClose is best-effort because Client.Send is asynchronous.
		_ = r.client.PaneClose(r.paneID)
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
