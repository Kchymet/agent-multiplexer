package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"amux/internal/accounts"
	"amux/internal/core"
)

// Now returns a unix-millis timestamp.
func Now() int64 { return time.Now().UnixMilli() }

// RootDir is the container directory for a root session's sub-worktrees.
func RootDir(rootID string) string { return filepath.Join(core.SessionsDir(), rootID) }

// CoordinatorDir is the private sandbox for a workgroup coordinator. It is a
// child of the workgroup container, but never the container itself: member
// sandboxes may remain direct children of RootDir (including after DB-only
// moves), so mounting RootDir would expose both current and former members.
func CoordinatorDir(rootID string) string {
	return filepath.Join(RootDir(rootID), "coordinator")
}

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

// LegacyRegistryError reports a legacy JSON registry that could not be parsed.
// Nothing is imported and the file is left in place.
type LegacyRegistryError struct {
	Path string
	Err  error
}

func (e *LegacyRegistryError) Error() string {
	return fmt.Sprintf("legacy registry %s: %v", e.Path, e.Err)
}

func (e *LegacyRegistryError) Unwrap() error { return e.Err }

// legacyRegistryImportVersion is the marker row importLegacy commits together
// with the imported rows.
const legacyRegistryImportVersion = 1

// importLegacy one-time imports the old JSON registry into the DB, then renames
// the JSON aside as a backup. It never imports into a store that already has
// sessions. Each old (multi-repo, single-agent) workspace becomes a root
// container plus one sub-session that preserves its dir + claude session.
//
// The import is one write transaction: it rechecks the marker and the empty
// store under the writer reservation, reads the file, writes every row and the
// marker, and commits them together, so racing first opens (in any process)
// import at most once and nobody sees a partial import. The file is renamed
// only after the commit. A crash or failure before the commit leaves the store
// and the file as they were, and the next Open imports again. A crash after it
// leaves the marker, so the next Open just moves the file aside.
func (d *DB) importLegacy() error {
	regPath := core.RegistryPath()
	if _, err := os.Stat(regPath); errors.Is(err, fs.ErrNotExist) {
		// The file is only removed after an import has committed, so there is
		// nothing left to import.
		return nil
	} else if err != nil {
		return err
	}
	imported := false
	if err := d.writeTx(func(q querier) error {
		var marked, sessions int
		if err := q.QueryRow(`SELECT count(*) FROM legacy_registry_import`).Scan(&marked); err != nil {
			return err
		}
		if marked > 0 {
			imported = true // committed earlier; only the rename is left
			return nil
		}
		if err := q.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
			return err
		}
		if sessions > 0 {
			return nil // already have sessions; don't import into them
		}
		b, err := os.ReadFile(regPath)
		if err != nil {
			return err
		}
		var lg legacy
		if err := json.Unmarshal(b, &lg); err != nil {
			return &LegacyRegistryError{Path: regPath, Err: err}
		}
		if err := importLegacyRows(q, lg); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO legacy_registry_import(version) VALUES(?)`, legacyRegistryImportVersion); err != nil {
			return err
		}
		imported = true
		return nil
	}); err != nil || !imported {
		return err
	}
	// Keep the registry as a backup, don't delete it. Another opener may have
	// moved it aside already; any other failure is reported and retried by the
	// next Open (the marker prevents a second import).
	if err := os.Rename(regPath, regPath+".migrated"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("legacy registry imported, but moving it aside failed: %w", err)
	}
	return nil
}

func importLegacyRows(q querier, lg legacy) error {
	for _, r := range lg.Repos {
		if err := putRepoRow(q, Repo{Name: r.Name, Source: r.Source, GitDir: r.GitDir}); err != nil {
			return err
		}
	}
	var sessions []Session
	for _, w := range lg.Workspaces {
		mode := NormalizeMode(w.Mode)
		// root container
		root := Session{
			ID: w.ID, RootID: "", Name: w.Name, Mode: mode,
			Dir: w.Dir, Created: w.Created, Scope: ScopeWork,
		}
		if err := putSessionRow(q, root); err != nil {
			return err
		}
		id, err := newIDIn(q)
		if err != nil {
			return err
		}
		// one sub preserving the legacy combined dir + claude session. An empty
		// agent is left empty (not defaulted): every reader canonicalizes "" to the
		// default kind via the agent registry, so the persistence layer needn't know
		// the default's spelling.
		sub := Session{
			ID: id, RootID: w.ID, Agent: w.Agent, Mode: mode,
			Repo: strings.Join(w.Repos, ","), Dir: w.Dir,
			ClaudeID: w.SessionID, Prompt: w.InitialPrompt, Created: w.Created,
		}
		if err := putSessionRow(q, sub); err != nil {
			return err
		}
		sessions = append(sessions, root, sub)
	}
	// Bind accounts before the rows commit, as PutSession does. If the
	// transaction then rolls back, the bindings name sessions that do not exist,
	// which nothing reads; the retry binds the sessions it actually writes.
	bindings := make(map[string]string, len(sessions))
	for _, s := range sessions {
		bindings[s.ID] = s.Agent
	}
	if len(bindings) == 0 {
		return nil
	}
	return accounts.BindAll(bindings)
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
	ScopeWork = "work" // coordinator workgroup: root + N agents, optional repo ceiling
	ScopeRepo = "repo" // repository management home; legacy wrappers migrate to work
)

// Session roles: the built-in "default sessions" every container hosts. They
// mirror the published harnessproto Role* values (store sits below core in the
// import graph, so the strings are restated here; harnessproto is the wire's
// source of truth and a test pins the two in step).
//
//   - RoleConsole: the machine-wide amux console (Mode == ModeConsole; synthetic).
//   - RoleCoordinator: a workgroup root's own session — the coordinator of its
//     agents, whether the workgroup is granted one repo or several. Its id is
//     the workgroup id and its sandbox is a dedicated coordinator dir beside
//     the members' sandboxes.
//   - RoleRepo: a tracked repo's home session — the long-lived context for the
//     repository management notes. Its id is the repo name (see
//     RepoHomeID) and it is a repo-scoped root with no members of its own.
//   - RoleAgent ("") : an ordinary agent (a member of a workgroup). Every root
//     hosts a session; the hidden single-member repo root of older daemons is
//     promoted into a workgroup when the store is opened for writing.
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
// stored: the console by its mode, a repo home by being the repo-scoped root
// whose id IS its repo name, and every other root by being a workgroup — its
// coordinator.
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
