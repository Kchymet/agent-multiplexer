package source

import (
	"context"

	"amux/internal/core"
)

// Hermes is a scaffolded adapter for Nous Hermes sessions. It is intentionally
// inert in v1 (the daemon does not register it) but pins the integration point:
// `hermes sessions list` (SQLite store) and `hermes kanban list` (task board).
//
// To enable: implement Poll to shell out to hermes, normalize rows to
// core.Session{Source: "hermes", ...}, and register it in daemon.Default().
type Hermes struct{}

func NewHermes() *Hermes { return &Hermes{} }

func (h *Hermes) Name() string { return "hermes" }

func (h *Hermes) Poll(ctx context.Context) ([]core.Session, error) {
	// TODO(v2): parse `hermes sessions list` and `hermes kanban list`.
	return nil, nil
}
