package source

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// untrackedTTL bounds how long a Claude session amux didn't launch stays on the
// rail after its last hook event, so one that crashed without a SessionEnd
// eventually drops off instead of lingering forever.
const untrackedTTL = 12 * time.Hour

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

	// Claude sessions amux manages, by id and by dir, so untracked enumeration
	// can skip them (the dir check also catches legacy sessions with no pinned id).
	tracked := map[string]bool{console.SessionID: true}
	trackedDirs := map[string]bool{console.Dir(): true}

	// Control console, pinned first.
	consoleState := agentState(running[console.ID], console.SessionID)
	out = append(out, core.Session{
		ID: console.ID, Title: "amux console", Source: "workspace", Kind: "claude",
		Mode: "console", State: consoleState, Status: stateLabel(consoleState) + " · configure amux",
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
			subStates[i] = agentState(running[s.ID], s.ClaudeID)
			if stateRank(subStates[i]) > stateRank(rootState) {
				rootState = subStates[i]
			}
		}
		out = append(out, core.Session{
			ID: r.ID, Title: r.Display(), Source: "workspace", Section: core.SectionWorkspaces,
			IsRoot: true, Mode: r.Mode,
			State:     rootState,
			Status:    fmt.Sprintf("%s · %d agent%s", stateLabel(rootState), len(subs), plural(len(subs))),
			Cwd:       r.Dir,
			CanAttach: true, // Enter opens all sub-sessions
			CanKill:   true, // delete the whole root
		})
		for i, s := range subs {
			if s.ClaudeID != "" {
				tracked[s.ClaudeID] = true
			}
			if s.Dir != "" {
				trackedDirs[s.Dir] = true
			}
			out = append(out, core.Session{
				ID: s.ID, Title: subLabel(s), Source: "workspace", Section: core.SectionWorkspaces,
				RootID: s.RootID, Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
				State:     subStates[i],
				Status:    stateLabel(subStates[i]) + subSuffix(s),
				Cwd:       s.Dir,
				WindowID:  running[s.ID],
				CanAttach: true,
				CanKill:   true,
			})
		}
	}

	// Tracked repositories: a quick launcher — Enter starts a workspace from one.
	if repos, err := db.Repos(); err == nil {
		for _, r := range repos {
			out = append(out, core.Session{
				ID: r.Name, Title: repoTitle(r), Source: "workspace", Section: core.SectionRepos,
				Kind: "repo", Cwd: r.GitDir,
			})
		}
	}

	// Claude sessions amux didn't launch (visible because the status hooks are
	// user-level), shown read-only at the bottom.
	out = append(out, untrackedRows(tracked, trackedDirs)...)
	return out, nil
}

// untrackedRows lists Claude sessions amux didn't launch: any with reported hook
// activity whose id (and dir) isn't tracked. They have no tmux window here, so
// they're informational only; ended (idle) and stale sessions are dropped.
func untrackedRows(tracked, trackedDirs map[string]bool) []core.Session {
	var out []core.Session
	now := time.Now().UnixMilli()
	for id, rec := range core.AllHookStates() {
		if tracked[id] || trackedDirs[rec.Cwd] || rec.State == core.StateIdle {
			continue // ours (by id or dir), or a session that has ended
		}
		if rec.Updated > 0 && now-rec.Updated > untrackedTTL.Milliseconds() {
			continue // stale: likely crashed without a SessionEnd
		}
		out = append(out, core.Session{
			ID:        shortID(id),
			Title:     untrackedTitle(rec.Cwd, id),
			Source:    "workspace",
			Section:   core.SectionDetached,
			Kind:      "claude",
			Mode:      "external",
			State:     rec.State,
			Status:    stateLabel(rec.State) + " · untracked",
			Cwd:       rec.Cwd,
			StartedAt: rec.Updated,
		})
	}
	return out
}

// repoTitle shows a tracked repo as "org/repo" for remote sources, falling back
// to its bare name for local paths or anything without a clear owner segment.
func repoTitle(r store.Repo) string {
	s := r.Source
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return r.Name // local path: no meaningful owner
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:] // drop scheme
	}
	s = strings.ReplaceAll(s, ":", "/") // normalize scp-style git@host:org/repo
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	var parts []string
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return r.Name
}

// shortID abbreviates a session uuid for display.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// untrackedTitle labels an untracked session by its working directory, falling
// back to its short id.
func untrackedTitle(cwd, id string) string {
	if b := filepath.Base(cwd); b != "" && b != "." && b != string(filepath.Separator) {
		return b
	}
	return shortID(id)
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

// agentState classifies a session's activity. The fine-grained states come from
// Claude Code's hooks (see claudecfg.InstallHooks), which write the current
// state per session as the agent's turn lifecycle fires:
//   - StateIdle:    no live tmux window (the agent isn't running)
//   - StateRunning: a turn is in flight
//   - StateWaiting: blocked on a prompt awaiting the user
//   - StateReady:   turn finished / freshly launched, ready for input
//   - StateUnknown: window live but no hook data yet (a pre-hook session, or one
//     that hasn't fired its first event) — shown as a less certain "running".
func agentState(win, claudeID string) string {
	if win == "" {
		return core.StateIdle
	}
	if rec, ok := core.HookState(claudeID); ok {
		switch rec.State {
		case core.StateRunning, core.StateWaiting, core.StateReady, core.StateIdle:
			return rec.State
		}
	}
	return core.StateUnknown
}

// stateLabel is the word shown to the user. Unknown reads as "running": the
// agent is live, we just lack granular hook data (the rail tints it differently).
func stateLabel(state string) string {
	if state == core.StateUnknown {
		return core.StateRunning
	}
	return state
}

// stateRank orders states by how much they want the user's attention, highest
// first, so a root can inherit its most demanding child's state.
func stateRank(state string) int {
	switch state {
	case core.StateWaiting:
		return 4
	case core.StateRunning:
		return 3
	case core.StateUnknown:
		return 2
	case core.StateReady:
		return 1
	default: // idle
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
