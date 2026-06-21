package source

import (
	"context"
	"fmt"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// Workspace is the rail's primary source: it lists tracked workspaces and marks
// which are currently open (have a tagged tmux window).
type Workspace struct{}

func NewWorkspace() *Workspace { return &Workspace{} }

func (w *Workspace) Name() string { return "workspace" }

func (w *Workspace) Poll(ctx context.Context) ([]core.Session, error) {
	reg, err := store.Load()
	if err != nil {
		return nil, err
	}
	running := tmuxctl.WorkspaceWindows(ctx)

	sessions := make([]core.Session, 0, len(reg.Workspaces)+1)

	// The amux control console is always pinned first.
	cstate := "idle"
	if running[console.ID] != "" {
		cstate = "running"
	}
	sessions = append(sessions, core.Session{
		ID:        console.ID,
		Title:     "amux console",
		Source:    "workspace",
		Kind:      "claude",
		Mode:      "console",
		Status:    cstate + " · configure amux",
		Cwd:       console.Dir(),
		WindowID:  running[console.ID],
		CanAttach: true,
		CanKill:   false, // the console can't be deleted
	})

	for _, ws := range reg.Workspaces {
		win := running[ws.ID]
		state := "idle"
		if win != "" {
			state = "running"
		}
		mode := ws.Mode
		if mode == "" {
			mode = store.ModeTask
		}
		sessions = append(sessions, core.Session{
			ID:        ws.ID,
			Title:     ws.Display(), // name if set, else id
			Source:    "workspace",
			Kind:      ws.Agent,
			Mode:      mode,
			Status:    fmt.Sprintf("%s · %s · %d repo%s", mode, state, len(ws.Repos), plural(len(ws.Repos))),
			Cwd:       ws.Dir,
			WindowID:  win,
			StartedAt: ws.Created,
			CanAttach: true, // Enter opens (running) or starts (idle) the workspace
			CanKill:   true, // x deletes the workspace
		})
	}
	return sessions, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
