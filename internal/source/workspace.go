package source

import (
	"context"
	"fmt"
	"strings"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// Workspace is the rail's source: the control console, then each root session
// with its sub-sessions nested underneath.
type Workspace struct{}

func NewWorkspace() *Workspace { return &Workspace{} }

func (w *Workspace) Name() string { return "workspace" }

func (w *Workspace) Poll(ctx context.Context) ([]core.Session, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	running := tmuxctl.WorkspaceWindows(ctx)
	var out []core.Session

	// Control console, pinned first.
	out = append(out, core.Session{
		ID: console.ID, Title: "amux console", Source: "workspace", Kind: "claude",
		Mode: "console", Status: stateOf(running[console.ID]) + " · configure amux",
		Cwd: console.Dir(), WindowID: running[console.ID], CanAttach: true, CanKill: false,
	})

	roots, err := db.Roots()
	if err != nil {
		return nil, err
	}
	for _, r := range roots {
		subs, err := db.Children(r.ID)
		if err != nil {
			return nil, err
		}
		anyRunning := false
		for _, s := range subs {
			if running[s.ID] != "" {
				anyRunning = true
			}
		}
		rootWin := ""
		if anyRunning {
			rootWin = "running" // sentinel: at least one sub is live (for glyph)
		}
		out = append(out, core.Session{
			ID: r.ID, Title: r.Display(), Source: "workspace", IsRoot: true, Mode: r.Mode,
			Status:    fmt.Sprintf("%s · %d agent%s", stateOf(rootWin), len(subs), plural(len(subs))),
			Cwd:       r.Dir,
			CanAttach: true, // Enter opens all sub-sessions
			CanKill:   true, // delete the whole root
		})
		for _, s := range subs {
			out = append(out, core.Session{
				ID: s.ID, Title: subLabel(s), Source: "workspace", RootID: s.RootID,
				Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
				Status:    stateOf(running[s.ID]) + subSuffix(s),
				Cwd:       s.Dir,
				WindowID:  running[s.ID],
				CanAttach: true,
				CanKill:   true,
			})
		}
	}
	return out, nil
}

func subLabel(s store.Session) string {
	if strings.TrimSpace(s.Name) != "" {
		return s.Name
	}
	if s.Repo != "" {
		return s.Repo
	}
	return s.ID
}

func subSuffix(s store.Session) string {
	if s.Branch != "" {
		return " · " + s.Branch
	}
	if s.Model != "" {
		return " · " + s.Model
	}
	return ""
}

func stateOf(win string) string {
	if win != "" {
		return "running"
	}
	return "idle"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
