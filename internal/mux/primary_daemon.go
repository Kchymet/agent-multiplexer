package mux

import (
	"context"
	"fmt"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/panespec"
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

func (daemonPrimary) LaunchSpec(ctx context.Context, id string) (panespec.LaunchSpec, error) {
	return daemonCall(ctx, func(c *daemon.Client) (panespec.LaunchSpec, error) { return c.LaunchSpec(id) })
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
