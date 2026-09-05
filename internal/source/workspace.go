package source

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
)

// untrackedTTL bounds how long a Claude session amux didn't launch stays on the
// rail after its last hook event, so one that crashed without a SessionEnd
// eventually drops off instead of lingering forever.
const untrackedTTL = 12 * time.Hour

// maxArchivedRows caps how many archived agents the rail shows. Archived agents
// accumulate without bound, so the ARCHIVED section can grow to dwarf the active
// rail; we surface only the most recently archived. Older ones stay in the store
// and remain reachable via `amux wg`.
const maxArchivedRows = 8

// Workspace is the rail's source: the control console, then each root session
// with its sub-sessions nested underneath, then each tracked repo with its
// one-off agents. The console, every work-scoped root, and every repo row is
// itself a session — the built-in default sessions (store.Role*): the console,
// the workgroup's coordinator, the repo's home — so those rows carry a runtime,
// caps, a role, and their own activity state, and can be opened and steered
// like an agent.
type Workspace struct {
	// engineLive, if set, reports which agent ids are running in the daemon's
	// engine. An agent is "live" if it's in the engine, which is what lights it
	// up on the rail.
	engineLive func() map[string]bool
	// controlMode, if set, reports a session's control mode (harnessproto
	// ControlMode*) — "structured" for a session under the App Server supervisor,
	// "" (pty) otherwise (AGE-181). nil ⇒ every row is pty, as before.
	controlMode func(id string) string
}

func NewWorkspace() *Workspace { return &Workspace{} }

// SetLiveness installs the engine-liveness probe (see Workspace.engineLive).
func (w *Workspace) SetLiveness(f func() map[string]bool) { w.engineLive = f }

// SetControlMode installs the control-mode probe (see Workspace.controlMode).
func (w *Workspace) SetControlMode(f func(id string) string) { w.controlMode = f }

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

	// Control console, pinned first: the machine-wide default session.
	consoleState := agentState(liveOf(console.ID), console.Session())
	out = append(out, w.withCaps(core.Session{
		ID: console.ID, Title: "amux console", Source: "workspace", Kind: agent.DefaultKind(),
		Mode: store.ModeConsole, Role: store.RoleConsole,
		State: consoleState, Status: stateLabel(consoleState) + " · amux-wide",
		Cwd: console.Dir(), CanAttach: true, CanKill: false,
	}, true)) // console resolves in steer.go — steerable

	roots, err := db.Roots()
	if err != nil {
		return nil, err
	}
	// Repo-scoped (single-member) workgroups don't get their own row — their agent
	// renders nested under the repo header in the REPOS section. Collect them by
	// repo while passing over the roots; work-scoped roots render inline here.
	// Archived agents/workgroups are pulled aside into a collapsed ARCHIVED section.
	repoAgents := map[string][]store.Session{}
	repoHomes := map[string]store.Session{} // a repo's home session, by repo name
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
		if r.Role() == store.RoleRepo {
			// A repo's home session renders AS the repo header (below), not as a
			// workgroup: its id is the repo name, which is already the header's id.
			track(r)
			repoHomes[r.Repo] = r
			continue
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
		// The root row is the workgroup's coordinator session. Its own activity
		// leads its status; the row's state is the most demanding of its own and
		// its agents', so a workgroup with a blocked member still floats up.
		track(r)
		ownState := agentState(liveOf(r.ID), r)
		subStates := make([]string, len(active))
		rootState := ownState
		for i, s := range active {
			subStates[i] = agentState(liveOf(s.ID), s)
			if stateRank(subStates[i]) > stateRank(rootState) {
				rootState = subStates[i]
			}
		}
		out = append(out, w.withCaps(core.Session{
			ID: r.ID, Title: r.Display(), Source: "workspace", Section: core.SectionWorkgroups,
			IsRoot: true, Kind: agent.Canonical(r.Agent), Mode: store.NormalizeMode(r.Mode),
			Role:      store.RoleCoordinator,
			State:     rootState,
			Status:    fmt.Sprintf("%s · %d agent%s", stateLabel(ownState), len(active), plural(len(active))),
			Cwd:       containerDir(r),
			CanAttach: true, // opens the coordinator
			CanKill:   true, // delete the whole root
		}, true)) // the coordinator resolves in steer.go — steerable
		for i, s := range active {
			out = append(out, w.withCaps(core.Session{
				ID: s.ID, Title: agentLabel(s), Source: "workspace", Section: core.SectionWorkgroups,
				RootID: s.RootID, Kind: agent.Canonical(s.Agent), Mode: s.Mode, Repos: s.Repo,
				State:     subStates[i],
				Status:    stateLabel(subStates[i]) + subSuffix(s) + noticeSuffix(s) + configSuffix(s),
				Cwd:       s.Dir,
				CanAttach: true,
				CanKill:   true,
			}, true)) // tracked, active workgroup agent — steerable
		}
	}

	// Tracked repositories, each a container for its repo-scoped agents (nested
	// directly beneath, so a single-repo agent shows here, never under WORKGROUPS).
	// The repo row is the repo's home session (id = repo name). A repo tracked
	// before default sessions has no home row yet; the daemon creates it on first
	// open (wsops.ResolveSession), so the header is still openable and steerable
	// and carries the runtime it will have.
	if repos, err := db.Repos(); err == nil {
		for _, r := range repos {
			home, ok := repoHomes[r.Name]
			if !ok {
				home = store.Session{ID: store.RepoHomeID(r.Name), Agent: agent.DefaultKind(), Scope: store.ScopeRepo, Repo: r.Name, Mode: store.ModeInteractive}
			}
			st := agentState(liveOf(home.ID), home)
			out = append(out, w.withCaps(core.Session{
				ID: r.Name, Title: repoTitle(r), Source: "workspace", Section: core.SectionRepos,
				Kind: "repo", Mode: store.NormalizeMode(home.Mode), Role: store.RoleRepo,
				State: st, Status: stateLabel(st) + " · repo home",
				Cwd: containerDir(home), CanAttach: true,
			}, true)) // the home resolves in steer.go — steerable
			for _, s := range repoAgents[r.Name] {
				st := agentState(liveOf(s.ID), s)
				out = append(out, w.withCaps(core.Session{
					ID: s.ID, Title: agentLabel(s), Source: "workspace", Section: core.SectionRepos,
					RootID: r.Name, Kind: agent.Canonical(s.Agent), Mode: s.Mode, Repos: s.Repo,
					State:     st,
					Status:    stateLabel(st) + subSuffix(s) + noticeSuffix(s) + configSuffix(s),
					Cwd:       s.Dir,
					CanAttach: true,
					CanKill:   true,
				}, true)) // tracked, active repo agent — steerable
			}
		}
	}

	// Archived agents, collapsed at the bottom — out of the way but reviewable and
	// restorable (press the archive key again, or `amux wg unarchive <id>`). Cap
	// the list at the most recently archived so it can't overrun the active rail;
	// the rest linger in the store, reachable via `amux wg`.
	for _, s := range recentArchived(archived) {
		out = append(out, w.withCaps(core.Session{
			ID: s.ID, Title: agentLabel(s), Source: "workspace", Section: core.SectionArchived,
			Kind: agent.Canonical(s.Agent), Mode: s.Mode,
			State: core.StateIdle, Status: "archived" + subSuffix(s), Archived: true,
			Cwd: s.Dir, CanAttach: true, CanKill: true,
		}, false)) // archived / observe-only — not steerable
	}

	// Claude sessions amux didn't launch (visible because the status hooks are
	// user-level), shown read-only at the bottom.
	out = append(out, w.untrackedRows(tracked, trackedDirs)...)
	return out, nil
}

// withCaps stamps a session row with its runtime identity and the control
// capabilities amux can serve for it (AGE-178), so a remote orchestrator gates
// its affordances on the row's *effective control path* rather than on the
// runtime name alone. It is applied to rows that host a session: every agent,
// and the container rows that are default sessions (the console, a workgroup's
// coordinator, a repo's home) — a repo row keeps its structural Kind ("repo"),
// so its Runtime is resolved from the home session, not from Kind.
//
// Runtime identity is always preserved. Caps, however, are gated on `steerable`:
// the daemon's steer handler (internal/daemon/steer.go) resolves only the console
// and live *tracked* store sessions, so a detached/external row (untrackedRows —
// no store row, rejected "no such session") and an archived/observe-only row can
// NOT be driven even though their runtime's TUI keys exist. Those rows carry an
// explicit non-nil ALL-FALSE SessionCaps: a consumer disables their controls
// rather than offering an affordance the daemon would refuse. A steerable row
// carries the honest per-kind caps (agent.CapsFor).
//
// These are the daemon-owned TUI control-path caps. Any broker-owned app-server
// adapter capability (e.g. a CODEX_DRIVER app-server path) is a separate surface
// the consumer layers on; it must not overwrite this daemon control path.
func (w *Workspace) withCaps(s core.Session, steerable bool) core.Session {
	s.Runtime = agent.Canonical(s.Kind)
	if s.Kind == "repo" {
		s.Runtime = agent.DefaultKind() // the repo home's runtime; Kind stays the structural label
	}
	var caps core.SessionCaps
	if steerable {
		caps = agent.CapsFor(s.Runtime)
	}
	s.Caps = &caps
	// ControlMode says HOW those caps are delivered (§2.2): a supervised session is
	// "structured", everything else pty (the empty default). Only a steerable row —
	// one the daemon can actually drive — is ever structured; a detached/archived
	// row stays pty even if a stale supervisor probe existed.
	if steerable && w.controlMode != nil {
		s.ControlMode = w.controlMode(s.ID)
	}
	return s
}

// containerDir is a container session's sandbox: the stored dir, or — for a
// root that predates default sessions and will get one on first open — the
// container dir it will be given (see wsops.ResolveSession).
func containerDir(s store.Session) string {
	if s.Dir != "" {
		return s.Dir
	}
	return store.RootDir(s.ID)
}

// recentArchived returns the most recently archived sessions (by ArchivedAt,
// newest first), capped at maxArchivedRows. Older archived sessions are dropped
// from the rail but remain in the store. It sorts a copy so the caller's slice is
// untouched. Sessions archived before ArchivedAt was tracked have ArchivedAt 0 and
// fall back to Created order, so they sort below any freshly archived ones.
func recentArchived(archived []store.Session) []store.Session {
	sorted := append([]store.Session(nil), archived...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ai, aj := sorted[i].ArchivedAt, sorted[j].ArchivedAt
		if ai != aj {
			return ai > aj
		}
		return sorted[i].Created > sorted[j].Created
	})
	if len(sorted) > maxArchivedRows {
		sorted = sorted[:maxArchivedRows]
	}
	return sorted
}

// untrackedRows lists Claude sessions amux didn't launch: any with reported hook
// activity whose id (and dir) isn't tracked. amux doesn't host them, so they're
// informational only; ended (idle) and stale sessions are dropped.
func (w *Workspace) untrackedRows(tracked, trackedDirs map[string]bool) []core.Session {
	var out []core.Session
	now := time.Now().UnixMilli()
	for id, rec := range core.AllHookStates() {
		if tracked[id] || trackedDirs[rec.Cwd] || rec.State == core.StateIdle {
			continue // ours (by id or dir), or a session that has ended
		}
		if rec.Updated > 0 && now-rec.Updated > untrackedTTL.Milliseconds() {
			continue // stale: likely crashed without a SessionEnd
		}
		out = append(out, w.withCaps(core.Session{
			ID:        shortID(id),
			Title:     untrackedTitle(rec.Cwd, id),
			Source:    "workspace",
			Section:   core.SectionDetached,
			Kind:      agent.DefaultKind(),
			Mode:      "external",
			State:     rec.State,
			Status:    stateLabel(rec.State) + " · untracked",
			Cwd:       rec.Cwd,
			StartedAt: rec.Updated,
		}, false)) // external/detached — steer.go can't resolve it, so not steerable
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

// agentLabel is the rail title for a leaf agent row — the line you read when
// selecting between agents. Prefer an explicit name, then a one-line summary of
// what the agent was asked to do, falling back to the short id for agents
// started without a prompt.
func agentLabel(s store.Session) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	if t := taskSummary(s.Prompt); t != "" {
		return t
	}
	return shortID(s.ID)
}

// taskSummary condenses an agent's initial prompt into a one-line rail label:
// the first non-blank line with its internal whitespace collapsed. Width
// truncation is left to the rail renderer. Empty for agents started without a
// prompt.
func taskSummary(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if t := strings.Join(strings.Fields(line), " "); t != "" {
			return t
		}
	}
	return ""
}

// noticeSuffix appends any pending rail warning for a session (e.g. a pinned
// conversation that couldn't be resumed) to its status line, so a silent
// fallback becomes visible on the rail.
func noticeSuffix(s store.Session) string {
	if n := core.Notice(s.ClaudeID); n != "" {
		return " · ⚠ " + n
	}
	return ""
}

// configSuffix flags an agent that has edited its private harness config (its
// copy of the user's ~/.claude / $CODEX_HOME template) so the change is visible
// on the rail as soon as the daemon's poll sees it — the feedback loop that lets
// the user decide whether to propagate it (`amux sandbox drift`). Only edits
// awaiting a decision count; a template the user changed under the agent is not
// the agent's doing and is left to `amux sandbox drift` to list.
func configSuffix(s store.Session) string {
	spec, ok := agent.HarnessFor(s.Agent).Config(s)
	if !ok {
		return ""
	}
	if n := cfghome.Pending(cfghome.Summary(spec)); n > 0 {
		return fmt.Sprintf(" · ⚙ %d config edit%s", n, plural(n))
	}
	return ""
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

// agentState classifies a session's activity. Claude's fine-grained states come
// from its hooks (see claudecfg.InstallHooksIn), which write the current state per
// session as the agent's turn lifecycle fires:
//   - StateIdle:    not running (no engine instance)
//   - StateRunning: a turn is in flight
//   - StateWaiting: blocked on a prompt awaiting the user
//   - StateReady:   turn finished / freshly launched, ready for input
//   - StateUnknown: live but no hook data yet (a pre-hook session, or one that
//     hasn't fired its first event) — shown as a less certain "running".
//
// A harness with no hook stream (e.g. Codex) can't report those turn states, so
// rather than showing a stale hook state that will never arrive we ask its Harness
// for a coarse activity signal (Codex infers one from rollout freshness) and map
// it onto running/ready — degrading honestly to "the engine has a live instance".
func agentState(alive bool, s store.Session) string {
	if !alive {
		return core.StateIdle
	}
	// The rail state word is the harness's call: Claude reports its fine-grained
	// hook states directly; a harness with no hook stream degrades honestly to
	// running/ready via its coarse activity signal. HarnessFor is the one switch.
	return agent.HarnessFor(s.Agent).RailState(s)
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
