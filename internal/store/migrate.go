package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"amux/internal/core"
)

// Now returns a unix-millis timestamp.
func Now() int64 { return time.Now().UnixMilli() }

// RootDir is the container directory for a root session's sub-worktrees.
func RootDir(rootID string) string { return filepath.Join(core.SessionsDir(), rootID) }

// SubDir is the worktree directory for a sub-session under its root.
func SubDir(rootID, subID, repo, branch string) string {
	label := strings.Trim(Slug(repo)+"-"+Slug(branch), "-")
	if label == "" {
		label = subID
	}
	return filepath.Join(RootDir(rootID), label)
}

// AgentDir is an agent's base directory; it holds one worktree subdir per repo
// the agent works on, so the agent operates across its own worktrees only.
func AgentDir(rootID, agentID string) string {
	return filepath.Join(core.SessionsDir(), rootID, agentID)
}

// SplitRepos parses a comma-separated repo list (trimming blanks).
func SplitRepos(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JoinRepos serializes a repo list.
func JoinRepos(repos []string) string { return strings.Join(repos, ",") }

// BackfillWorkspaceRepos gives each root (workspace) an attached-repo list — the
// union of its agents' repos — for roots that don't have one yet. Idempotent.
func (d *DB) BackfillWorkspaceRepos() error {
	roots, err := d.Roots()
	if err != nil {
		return err
	}
	for _, r := range roots {
		if strings.TrimSpace(r.Repo) != "" {
			continue
		}
		subs, _ := d.Children(r.ID)
		seen := map[string]bool{}
		var union []string
		for _, s := range subs {
			for _, rp := range SplitRepos(s.Repo) {
				if !seen[rp] {
					seen[rp] = true
					union = append(union, rp)
				}
			}
		}
		if len(union) > 0 {
			_ = d.SetRepoScope(r.ID, JoinRepos(union)) // field-scoped, not a full-row upsert
		}
	}
	return nil
}

// legacy mirrors the old JSON registry shape (pre-SQLite).
type legacy struct {
	Repos []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
		GitDir string `json:"gitDir"`
	} `json:"repos"`
	Workspaces []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Agent         string   `json:"agent"`
		Mode          string   `json:"mode"`
		Repos         []string `json:"repos"`
		Dir           string   `json:"dir"`
		InitialPrompt string   `json:"initialPrompt"`
		SessionID     string   `json:"sessionId"`
		Created       int64    `json:"created"`
	} `json:"workspaces"`
}

// importLegacy one-time imports the old JSON registry into the DB, then renames
// the JSON aside as a backup. It is a no-op if the DB already has sessions or no
// registry file exists. Each old (multi-repo, single-agent) workspace becomes a
// root container plus one sub-session that preserves its dir + claude session.
func (d *DB) importLegacy() error {
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already have sessions; don't re-import
	}
	regPath := core.RegistryPath()
	b, err := os.ReadFile(regPath)
	if err != nil {
		return nil // no legacy registry
	}
	var lg legacy
	if err := json.Unmarshal(b, &lg); err != nil {
		return err
	}

	for _, r := range lg.Repos {
		_ = d.PutRepo(Repo{Name: r.Name, Source: r.Source, GitDir: r.GitDir})
	}
	for _, w := range lg.Workspaces {
		mode := NormalizeMode(w.Mode)
		// root container
		_ = d.PutSession(Session{
			ID: w.ID, RootID: "", Name: w.Name, Mode: mode,
			Dir: w.Dir, Created: w.Created, Scope: ScopeWork,
		})
		// one sub preserving the legacy combined dir + claude session. An empty
		// agent is left empty (not defaulted): every reader canonicalizes "" to the
		// default kind via the agent registry, so the persistence layer needn't know
		// the default's spelling.
		_ = d.PutSession(Session{
			ID: d.NewID(), RootID: w.ID, Agent: w.Agent, Mode: mode,
			Repo: strings.Join(w.Repos, ","), Dir: w.Dir,
			ClaudeID: w.SessionID, Prompt: w.InitialPrompt, Created: w.Created,
		})
	}

	_ = os.Rename(regPath, regPath+".migrated") // keep as backup, don't delete
	return nil
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// Session modes. The axis is who drives the session, not how long it runs:
//   - ModeTask: an autonomous session handed a specific job. It runs to
//     completion (implement a change, open a PR, babysit it) and self-reports
//     done. A long-lived job (e.g. a PR-review loop) is still a task — it just
//     never decides it's finished until stopped.
//   - ModeInteractive: a human-driven session. The user drives it turn by turn;
//     amux never auto-archives it (the human owns its lifecycle).
const (
	ModeTask        = "task"
	ModeInteractive = "interactive"
	// ModeConsole marks the built-in console session (a synthetic row, never
	// stored): the one session that is neither a task nor a plain interactive
	// agent. It doubles as the console's role marker (see Session.Role).
	ModeConsole = "console"
)

// modeLoopLegacy is the retired "loop" mode value. It predates the
// interactive/task split, where a loop is just a long-lived task; NormalizeMode
// folds any stored "loop" row into ModeTask so legacy sessions keep working.
const modeLoopLegacy = "loop"

// NormalizeMode maps a stored/incoming mode string to a current mode value:
// blank and the legacy "loop" fold into ModeTask; any other value (ModeTask,
// ModeInteractive, or a future mode) passes through unchanged.
func NormalizeMode(m string) string {
	switch strings.TrimSpace(m) {
	case "", ModeTask, modeLoopLegacy:
		return ModeTask
	default:
		return m
	}
}

// The agent-harness catalog (selectable harnesses + per-harness model lists) now
// lives behind the agent registry (agent.Harnesses / agent.HarnessFor), not in
// the persistence layer — store sits below the registry in the import graph and
// no longer needs to know a kind's spelling or model set.

// Workgroup scopes (root sessions only).
const (
	ScopeWork = "work" // cross-repo workgroup: root + N agents
	ScopeRepo = "repo" // single-repo, single-member workgroup nested under its repo
)

// Session roles: the built-in "default sessions" every container hosts. They
// mirror the published harnessproto Role* values (store sits below core in the
// import graph, so the strings are restated here; harnessproto is the wire's
// source of truth and a test pins the two in step).
//
//   - RoleConsole: the machine-wide amux console (Mode == ModeConsole; synthetic).
//   - RoleCoordinator: a work-scoped workgroup root's own session — the
//     coordinator of its agents. Its id is the workgroup id and its sandbox is
//     the workgroup's container dir, which holds every member's sandbox.
//   - RoleRepo: a tracked repo's home session — the long-lived context for the
//     one-off agents run against that repo. Its id is the repo name (see
//     RepoHomeID) and it is a repo-scoped root with no members of its own.
//   - RoleAgent ("") : an ordinary agent, or a bare container (a hidden
//     single-member repo root) that hosts no session.
const (
	RoleAgent       = ""
	RoleConsole     = "console"
	RoleCoordinator = "coordinator"
	RoleRepo        = "repo"
)

// RepoHomeID is the session id of a tracked repo's home session: the repo name
// itself, so the rail's repo row, the daemon's pane/steer lookups, and the store
// all address the same thing without a mapping table. Repo names never collide
// with minted ids (six hex chars) in practice, and NewID checks the store anyway.
func RepoHomeID(repo string) string { return repo }

// Role classifies a session (see the Role* constants). It is derived, not
// stored: the console by its mode, a coordinator by being a work-scoped root, a
// repo home by being the repo-scoped root whose id IS its repo name.
func (s Session) Role() string {
	switch {
	case s.Mode == ModeConsole:
		return RoleConsole
	case !s.IsRoot():
		return RoleAgent
	case s.Scope == ScopeRepo:
		if s.Repo != "" && s.ID == RepoHomeID(s.Repo) {
			return RoleRepo
		}
		return RoleAgent // a hidden single-member repo root: no session of its own
	default:
		return RoleCoordinator
	}
}

// IsContainerSession reports whether s is one of the built-in default sessions
// (console, coordinator, repo home) rather than an ordinary agent.
func (s Session) IsContainerSession() bool { return s.Role() != RoleAgent }
