package mux

import (
	"context"

	"amux/internal/core"
	"amux/internal/hostbackend"
)

// daemonPrimary is the mux's production Primary: the shared local host backend.
// Every operation authenticates a fresh connection to the singleton daemon
// (pinned TLS plus current host proof), which makes rotation/revocation an
// admission boundary. Keeping queries separate avoids trying to correlate
// results on the daemon's asynchronous snapshot stream.
type daemonPrimary struct{ backend hostbackend.HostDaemon }

func (p daemonPrimary) Snapshot(ctx context.Context) ([]core.Session, error) {
	return p.backend.Snapshot(ctx)
}

func (p daemonPrimary) Dispatch(ctx context.Context, action core.Action) (string, error) {
	return p.backend.Apply(ctx, action)
}

func (p daemonPrimary) OpenPane(ctx context.Context, request PaneRequest) (PaneRelay, error) {
	pane, err := p.backend.OpenPane(ctx, hostbackend.PaneRequest(request))
	if err != nil {
		return nil, err
	}
	return pane, nil
}

// joinPrimaryCall owns a primary callback's lifetime; see hostbackend.Join.
func joinPrimaryCall[T any](ctx context.Context, closeTransport func(), call func() (T, error)) (T, error) {
	return hostbackend.Join(ctx, closeTransport, call)
}
