// Package source defines the adapter interface that feeds the daemon and the
// concrete adapters for each agent backend. v1 ships the Claude adapter; the
// Hermes and generic-tmux adapters are scaffolded behind the same interface so
// they drop in without touching the daemon.
package source

import (
	"context"

	"amux/internal/core"
)

// Source is a pollable backend that reports the agent sessions it knows about.
// Implementations must be safe to call concurrently and should treat "backend
// not present" as an empty result, not an error.
type Source interface {
	// Name is a short identifier, e.g. "claude".
	Name() string
	// Poll returns the current set of normalized sessions.
	Poll(ctx context.Context) ([]core.Session, error)
}
