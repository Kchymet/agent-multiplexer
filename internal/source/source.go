// Package source defines the adapter interface that feeds the daemon and the
// concrete adapters for each agent backend. The workspace adapter surfaces the
// sessions amux manages; additional adapters (e.g. Hermes) drop in behind the
// same interface without touching the daemon.
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
