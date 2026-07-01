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
)

// untrackedTTL bounds how long a Claude session amux didn't launch stays on the
// rail after its last hook event, so one that crashed without a SessionEnd
// eventually drops off instead of lingering forever.
const untrackedTTL = 12 * time.Hour

// Workspace is the rail's source: the control console, then each root session
// with its sub-sessions nested underneath.
type Workspace struct {
	// engineLive, if set, reports which agent ids are running in the daemon's
	// engine. An agent is "live" if it's in the engine, which is what lights it
	// up on the rail.
	engineLive func() map[string]bool
}

func NewWorkspace() *Workspace { return &Workspace{} }

// SetLiveness installs the engine-liveness probe (see Workspace.engineLive).
func (w *Workspace) SetLiveness(f func() map[string]bool) { w.engineLive = f }

func (w *Workspace) Name() string { return "workspace" }

func (w *Workspace) Poll(ctx context.Context) ([]core.Session, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// An agent is "live" if it's running in the daemon's engine.
	var engineLive map[string]bool
	if w.engineLive != nil {
		engineLive = w.engineLive()
	}
	liveOf := func(id string) bool { return engineLive[id] }
	var out []core.Session

	// Claude sessions amux manages, by id and by dir, so untracked enumeration
	// can skip them (the dir check also catches legacy sessions with no pinned id).
	tracked := map[string]bool{console.SessionID: true}
	trackedDirs := map[string]bool{console.Dir(): true}

	// Control console, pinned first.
	consoleState := agentState(liveOf(console.ID), console.SessionID)
	out = append(out, core.Session{
		ID: console.ID, Title: "amux console", Source: "workspace", Kind: "claude",
		Mode: "console", State: consoleState, Status: stateLabel(consoleState) + " · configure amux",
		Cwd: console.Dir(), CanAttach: true, CanKill: false,
	})

	roots, err := db.Roots()
	if err != nil {
		return nil, err
	}
	// Repo-scoped (single-member) workgroups don't get their own row — their agent
	// renders nested under the repo header in the REPOS section. Collect them by
	// repo while passing over the roots; work-scoped roots render inline here.
	// Archived agents/workgroups are pulled aside into a collapsed ARCHIVED section.
	repoAgents := map[string][]store.Session{}
	var archived []store.Session
	track := func(s store.Session) {
		if s.ClaudeID != "" {
			tracked[s.ClaudeID] = true
		}
		if s.Dir != "" {
			trackedDirs[s.Dir] = true
		}
	}
	for _, r := range roots {
		subs, err := db.Children(r.ID)
		if err != nil {
			return nil, err
		}
		if r.Scope == store.ScopeRepo {
			for _, s := range subs {
				track(s)
				if s.Archived || r.Archived {
					archived = append(archived, s)
					continue
				}
				repoAgents[firstRepo(r.Repo)] = append(repoAgents[firstRepo(r.Repo)], s)
			}
			continue
		}
		// Work-scoped: an archived workgroup (or sub) is set aside; the rest render
		// inline, the root inheriting its most demanding child's state.
		var active []store.Session
		for _, s := range subs {
			track(s)
			if s.Archived || r.Archived {
				archived = append(archived, s)
				continue
			}
			active = append(active, s)
		}
		if r.Archived {
			continue
		}
		subStates := make([]string, len(active))
		rootState := core.StateIdle
		for i, s := range active {
			subStates[i] = agentState(liveOf(s.ID), s.ClaudeID)
			if stateRank(subStates[i]) > stateRank(rootState) {
				rootState = subStates[i]
			}
		}
		out = append(out, core.Session{
			ID: r.ID, Title: r.Display(), Source: "workspace", Section: core.SectionWorkgroups,
			IsRoot: true, Mode: r.Mode,
			State:     rootState,
			Status:    fmt.Sprintf("%s · %d agent%s", stateLabel(rootState), len(active), plural(len(active))),
			Cwd:       r.Dir,
			CanAttach: true, // Enter opens all sub-sessions
			CanKill:   true, // delete the whole root
		})
		for i, s := range active {
			out = append(out, core.Session{
				ID: s.ID, Title: subLabel(s), Source: "workspace", Section: core.SectionWorkgroups,
				RootID: s.RootID, Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
				State:     subStates[i],
				Status:    stateLabel(subStates[i]) + subSuffix(s),
				Cwd:       s.Dir,
				CanAttach: true,
				CanKill:   true,
			})
		}
	}

	// Tracked repositories, each a container for its repo-scoped agents (nested
	// directly beneath, so a single-repo agent shows here, never under WORKGROUPS).
	if repos, err := db.Repos(); err == nil {
		for _, r := range repos {
			out = append(out, core.Session{
				ID: r.Name, Title: repoTitle(r), Source: "workspace", Section: core.SectionRepos,
				Kind: "repo", Cwd: r.GitDir, CanAttach: true,
			})
			for _, s := range repoAgents[r.Name] {
				st := agentState(liveOf(s.ID), s.ClaudeID)
				out = append(out, core.Session{
					ID: s.ID, Title: repoAgentLabel(s), Source: "workspace", Section: core.SectionRepos,
					RootID: r.Name, Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
					State:     st,
					Status:    stateLabel(st) + subSuffix(s),
					Cwd:       s.Dir,
					CanAttach: true,
					CanKill:   true,
				})
			}
		}
	}

	// Archived agents, collapsed at the bottom — out of the way but reviewable and
	// restorable (press the archive key again, or `amux wg unarchive <id>`).
	for _, s := range archived {
		out = append(out, core.Session{
			ID: s.ID, Title: subLabel(s), Source: "workspace", Section: core.SectionArchived,
			Kind: defaultStr(s.Agent, "claude"), Mode: s.Mode,
			State: core.StateIdle, Status: "archived" + subSuffix(s),
			Cwd: s.Dir, CanAttach: true, CanKill: true,
		})
	}

	// Claude sessions amux didn't launch (visible because the status hooks are
	// user-level), shown read-only at the bottom.
	out = append(out, untrackedRows(tracked, trackedDirs)...)
	return out, nil
}

// untrackedRows lists Claude sessions amux didn't launch: any with reported hook
// activity whose id (and dir) isn't tracked. amux doesn't host them, so they're
// informational only; ended (idle) and stale sessions are dropped.
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

// firstRepo returns the first repo in a comma-separated list (a repo-scoped
// workgroup carries exactly one).
func firstRepo(list string) string {
	if r := store.SplitRepos(list); len(r) > 0 {
		return r[0]
	}
	return ""
}

// repoAgentLabel labels a repo-scoped agent nested under its repo header. The
// header already shows the repo, so fall back to the short id (not the repo) to
// keep the rows distinct when several agents share a repo.
func repoAgentLabel(s store.Session) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return shortID(s.ID)
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
//   - StateIdle:    not running (no engine instance)
//   - StateRunning: a turn is in flight
//   - StateWaiting: blocked on a prompt awaiting the user
//   - StateReady:   turn finished / freshly launched, ready for input
//   - StateUnknown: live but no hook data yet (a pre-hook session, or one that
//     hasn't fired its first event) — shown as a less certain "running".
func agentState(alive bool, claudeID string) string {
	if !alive {
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
