package source

import (
	"context"
	"strings"

	"amux/internal/core"
	"amux/internal/tmuxctl"
)

// Tmux surfaces every window running in the isolated amux server, so the rail
// shows ALL running sessions — shells, builds, hermes, anything — not just
// Claude agents. Windows that a richer adapter (Claude) already represents are
// de-duplicated by the daemon on WindowID.
type Tmux struct{}

func NewTmux() *Tmux { return &Tmux{} }

func (t *Tmux) Name() string { return "tmux" }

// win accumulates the representative (non-rail) pane for one window.
type win struct {
	name, cmd, path string
	haveMain        bool
}

func (t *Tmux) Poll(ctx context.Context) ([]core.Session, error) {
	// One pane per line; fields tab-separated. The rail pane is tagged @amx=rail
	// so we can pick the real work pane as the window's representative.
	const format = "#{window_id}\t#{window_name}\t#{@amx}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}"
	out, err := tmuxctl.Run(ctx, "list-panes", "-a", "-F", format)
	if err != nil || out == "" {
		// Server not running yet: nothing to report (not an error).
		return nil, nil
	}

	wins := map[string]*win{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			continue
		}
		id, name, amx, cmd, path, active := f[0], f[1], f[2], f[3], f[4], f[5]
		w := wins[id]
		if w == nil {
			w = &win{name: name}
			wins[id] = w
			order = append(order, id)
		}
		if amx == "rail" {
			continue // never represent a window by its rail pane
		}
		// Pick the active pane as representative; otherwise the first work pane.
		if !w.haveMain || active == "1" {
			w.cmd, w.path, w.haveMain = cmd, path, true
			if name != "" {
				w.name = name
			}
		}
	}

	sessions := make([]core.Session, 0, len(order))
	for _, id := range order {
		w := wins[id]
		if !w.haveMain {
			continue // window with only a rail pane (shouldn't happen)
		}
		sessions = append(sessions, core.Session{
			ID:        id,
			Title:     defaultStr(w.name, w.cmd),
			Source:    "tmux",
			Kind:      "window",
			Status:    w.cmd,
			Cwd:       w.path,
			WindowID:  id,
			CanAttach: true,
			CanKill:   true,
		})
	}
	return sessions, nil
}
