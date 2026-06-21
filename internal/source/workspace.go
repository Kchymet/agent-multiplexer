package source

import (
	"context"
	"fmt"
	"strings"

	"amux/internal/claudecfg"
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
	consoleState := agentState(ctx, running[console.ID], console.Dir(), console.SessionID)
	out = append(out, core.Session{
		ID: console.ID, Title: "amux console", Source: "workspace", Kind: "claude",
		Mode: "console", State: consoleState, Status: consoleState + " · configure amux",
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
		// State each sub once; the root inherits its most demanding child's state
		// (waiting > running > ready > idle), so a blocked agent surfaces upward.
		subStates := make([]string, len(subs))
		rootState := core.StateIdle
		for i, s := range subs {
			subStates[i] = agentState(ctx, running[s.ID], s.Dir, s.ClaudeID)
			if stateRank(subStates[i]) > stateRank(rootState) {
				rootState = subStates[i]
			}
		}
		out = append(out, core.Session{
			ID: r.ID, Title: r.Display(), Source: "workspace", IsRoot: true, Mode: r.Mode,
			State:     rootState,
			Status:    fmt.Sprintf("%s · %d agent%s", rootState, len(subs), plural(len(subs))),
			Cwd:       r.Dir,
			CanAttach: true, // Enter opens all sub-sessions
			CanKill:   true, // delete the whole root
		})
		for i, s := range subs {
			out = append(out, core.Session{
				ID: s.ID, Title: subLabel(s), Source: "workspace", RootID: s.RootID,
				Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
				State:     subStates[i],
				Status:    subStates[i] + subSuffix(s),
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

// agentState classifies a session's activity:
//   - StateIdle:    no live tmux window (the agent isn't running)
//   - StateRunning: window live and the agent has an active turn
//   - StateWaiting: window live, turn done, blocked on a prompt awaiting input
//   - StateReady:   window live, turn done, ready for the next message
//
// Sessions without a pinned Claude id can't be introspected, so a live window
// is assumed to be running (the prior behavior for those).
func agentState(ctx context.Context, win, dir, claudeID string) string {
	if win == "" {
		return core.StateIdle
	}
	if claudeID == "" {
		return core.StateRunning
	}
	if claudecfg.TurnActive(dir, claudeID) {
		return core.StateRunning
	}
	// Turn finished: distinguish "blocked on a prompt" from "ready" by looking at
	// what the agent's pane is actually showing.
	if pane := tmuxctl.WorkPane(ctx, win); pane != "" {
		if claudecfg.PromptBlocked(tmuxctl.CapturePane(ctx, pane)) {
			return core.StateWaiting
		}
	}
	return core.StateReady
}

// stateRank orders states by how much they want the user's attention, highest
// first, so a root can inherit its most demanding child's state.
func stateRank(state string) int {
	switch state {
	case core.StateWaiting:
		return 3
	case core.StateRunning:
		return 2
	case core.StateReady:
		return 1
	default: // idle / unknown
		return 0
	}
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
