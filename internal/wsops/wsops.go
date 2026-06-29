// Package wsops holds session lifecycle operations shared by the daemon (rail
// actions) and the CLI. A workspace (root) is a template that attaches repos but
// checks out nothing itself; its agents (subs) each work on a subset of those
// repos, one worktree per repo under the agent's own directory.
package wsops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/git"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// AgentSpec describes an agent to create under a workspace.
type AgentSpec struct {
	Repos  []string // subset of the workspace's repos this agent works on
	Agent  string   // defaults to "claude"
	Model  string   // optional model override
	Mode   string   // task | loop (defaults to task)
	Prompt string   // initial prompt
}

// CreateWorkspace creates a workspace (root) that attaches repos but checks out
// nothing itself. When defaultAgent is non-nil it also creates one default agent
// from that spec (its model, mode, and prompt are honored); a spec with no repos
// defaults to all of the workspace's. Pass nil to create no agent. Returns the
// workspace id.
func CreateWorkspace(ctx context.Context, name string, repos []string, defaultAgent *AgentSpec) (string, error) {
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()

	rootID := db.NewID()
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Name: strings.TrimSpace(name), Scope: store.ScopeWork,
		Mode: store.ModeTask, Repo: store.JoinRepos(repos), Created: store.Now(),
	}); err != nil {
		return "", err
	}
	if defaultAgent != nil {
		spec := *defaultAgent
		if len(spec.Repos) == 0 {
			spec.Repos = repos
		}
		if _, err := addAgent(ctx, db, rootID, spec); err != nil {
			return rootID, err
		}
	}
	return rootID, nil
}

// CreateRepoWorkgroup creates a single-member, repo-scoped workgroup for one repo
// plus its one agent. The wrapping root is hidden in the rail — the agent renders
// directly under the repo header. Returns the agent session.
func CreateRepoWorkgroup(ctx context.Context, repo string, spec AgentSpec) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	if _, ok, _ := db.Repo(repo); !ok {
		return store.Session{}, fmt.Errorf("unknown repo %q", repo)
	}
	rootID := db.NewID()
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Scope: store.ScopeRepo,
		Mode: defaultStr(spec.Mode, store.ModeTask), Repo: repo, Created: store.Now(),
	}); err != nil {
		return store.Session{}, err
	}
	spec.Repos = []string{repo}
	return addAgent(ctx, db, rootID, spec)
}

// AddAgent adds an agent (sub-session) to a workspace.
func AddAgent(ctx context.Context, rootID string, spec AgentSpec) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok || !root.IsRoot() {
		return store.Session{}, fmt.Errorf("no such workspace %q", rootID)
	}
	return addAgent(ctx, db, rootID, spec)
}

func addAgent(ctx context.Context, db *store.DB, rootID string, spec AgentSpec) (store.Session, error) {
	agentID := db.NewID()
	dir := store.AgentDir(rootID, agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Session{}, err
	}
	// Flat branch name (hyphen, not slash) so it never collides with a legacy
	// per-workspace branch `amux/<root>` — git refs can't have both `amux/<root>`
	// and `amux/<root>/<agent>` (file/directory conflict), which broke adding a
	// second agent to older workspaces.
	branch := "amux/" + rootID + "-" + agentID
	for _, repoName := range spec.Repos {
		repo, ok, err := db.Repo(repoName)
		if err != nil || !ok {
			return store.Session{}, fmt.Errorf("unknown repo %q", repoName)
		}
		if err := git.AddWorktree(ctx, repo.GitDir, filepath.Join(dir, repoName), branch); err != nil {
			return store.Session{}, err
		}
	}
	writeAgentGuide(dir, branch)
	a := store.Session{
		ID: agentID, RootID: rootID,
		Agent: defaultStr(spec.Agent, "claude"), Model: spec.Model,
		Mode: defaultStr(spec.Mode, store.ModeTask),
		Repo: store.JoinRepos(spec.Repos), Branch: branch, Dir: dir,
		ClaudeID: store.NewUUID(), Prompt: spec.Prompt, Created: store.Now(),
	}
	if err := db.PutSession(a); err != nil {
		return store.Session{}, err
	}
	return a, nil
}

// writeAgentGuide drops a CLAUDE.md into the agent's directory (its cwd) telling
// the agent to stay sandboxed to this dir and to keep its branch current with the
// remote. The agent dir is not a git repo, so this never dirties a worktree.
func writeAgentGuide(dir, branch string) {
	guide := fmt.Sprintf(`# amux agent — sandboxed workspace

This directory is your sandbox. It contains a git **worktree per repository** you
are assigned (the subdirectories here).

## Stay in your sandbox
- Only read and edit files **inside this directory** (your worktrees). Do not
  touch anything outside it: other agents' worktrees, the amux data dir, or any
  parent/clone of these repos.
- You are on branch `+"`%s`"+`. Commit only to this branch. Do not switch to or
  commit on the default branch (main/master), and do not push to it.

## Keep current with the remote
Each repo here is a worktree of a shared clone of its remote. Before starting,
and regularly as you work, refresh your branch from the remote — run inside each
repo subdirectory:

    git fetch origin && git rebase origin/HEAD

Resolve conflicts on your branch. This keeps you building on the latest remote,
not a stale snapshot.
`, branch)
	_ = os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(guide), 0o644)
}

// AttachRepo adds a repo to a workspace's attached set (template only; existing
// agents are unchanged — assign it to an agent by creating/adding one).
func AttachRepo(rootID, repo string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok || !root.IsRoot() {
		return fmt.Errorf("no such workspace %q", rootID)
	}
	if _, ok, _ := db.Repo(repo); !ok {
		return fmt.Errorf("unknown repo %q", repo)
	}
	repos := store.SplitRepos(root.Repo)
	for _, r := range repos {
		if r == repo {
			return nil // already attached
		}
	}
	root.Repo = store.JoinRepos(append(repos, repo))
	return db.PutSession(root)
}

// MoveAgent re-parents an agent into another workgroup. With an empty targetRootID
// it creates a new work-scoped workgroup seeded with the agent's repos. Only the
// agent's root_id changes — its worktree dir and branch are left where they are
// (they embed the old root id but are read from the stored values, so they keep
// working; physically moving git worktrees is avoided). An old single-member
// repo-scoped workgroup left empty is dropped (its container dir is left on disk
// because the moved agent's worktree still lives under it and is in use).
func MoveAgent(ctx context.Context, agentID, targetRootID string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	a, ok, err := db.GetSession(agentID)
	if err != nil || !ok {
		return fmt.Errorf("no such agent %q", agentID)
	}
	if a.IsRoot() {
		return fmt.Errorf("%q is a workgroup, not an agent", agentID)
	}
	oldRoot := a.RootID

	if strings.TrimSpace(targetRootID) == "" {
		newRoot, err := CreateWorkspace(ctx, defaultStr(a.Name, a.Repo), store.SplitRepos(a.Repo), nil)
		if err != nil {
			return err
		}
		targetRootID = newRoot
	} else {
		target, ok, _ := db.GetSession(targetRootID)
		if !ok || !target.IsRoot() {
			return fmt.Errorf("no such workgroup %q", targetRootID)
		}
		if target.Scope == store.ScopeRepo {
			return fmt.Errorf("cannot move into a repo-scoped workgroup; choose a work-scoped one (or omit a target to make a new one)")
		}
	}
	if targetRootID == oldRoot {
		return nil
	}

	a.RootID = targetRootID
	if err := db.PutSession(a); err != nil {
		return err
	}
	// Union the agent's repos into the destination so it lists them.
	if target, ok, _ := db.GetSession(targetRootID); ok {
		seen := map[string]bool{}
		set := store.SplitRepos(target.Repo)
		for _, r := range set {
			seen[r] = true
		}
		for _, r := range store.SplitRepos(a.Repo) {
			if !seen[r] {
				set = append(set, r)
				seen[r] = true
			}
		}
		target.Repo = store.JoinRepos(set)
		_ = db.PutSession(target)
	}
	// Drop the old workgroup if it's now empty (always true for a moved-out
	// single-member repo-scoped one).
	if kids, _ := db.Children(oldRoot); len(kids) == 0 {
		_ = db.DeleteSession(oldRoot)
	}
	return nil
}

// OpenByID opens a session: the console, a workspace (opens all its agents), or
// an agent (its window).
func OpenByID(ctx context.Context, id string) error {
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return err
		}
		return launch(ctx, console.Session())
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if s.IsRoot() {
		agents, err := db.Children(id)
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			return fmt.Errorf("workspace %q has no agents yet (add one with `a`)", id)
		}
		for _, a := range agents {
			if err := launch(ctx, a); err != nil {
				return err
			}
		}
		return nil
	}
	return launch(ctx, s)
}

// AgentCommand resolves everything needed to run an agent: its working dir, the
// extra environment (KEY=VALUE) to set, and the argv. It decides resume vs
// continue vs fresh the same way regardless of how the caller runs it — the
// native TUI execs this directly in an embedded PTY, while EnsureAgentSession
// hands it to tmux for the classic entrypoints.
func AgentCommand(s store.Session) (dir string, env, argv []string, err error) {
	if _, err := os.Stat(s.Dir); err != nil {
		return "", nil, nil, fmt.Errorf("session dir missing: %s", s.Dir)
	}
	if s.Agent == "" || s.Agent == "claude" {
		_ = claudecfg.TrustDir(s.Dir)
	}

	prompt := strings.TrimSpace(s.Prompt)
	var extra []string
	switch {
	case s.ClaudeID != "" && claudecfg.SessionExists(s.Dir, s.ClaudeID):
		extra = []string{"--resume", s.ClaudeID}
	case s.ClaudeID != "":
		extra = []string{"--session-id", s.ClaudeID}
		if prompt != "" {
			extra = append(extra, prompt)
		}
	case claudecfg.AnySession(s.Dir):
		extra = []string{"--continue"}
	default:
		if prompt != "" {
			extra = []string{prompt}
		}
	}
	argv, err = agent.Argv(s.Agent, s.Model, extra...)
	if err != nil {
		return "", nil, nil, err
	}
	env = []string{
		"AMUX_WORKGROUP=" + s.ID,
		"AMUX_WORKSPACE=" + s.ID, // back-compat alias for AMUX_WORKGROUP
		"AMUX_ROOT=" + s.RootID,
		"AMUX_SCOPE=" + agentScope(s.RootID),
		"AMUX_MODE=" + defaultStr(s.Mode, store.ModeTask),
		"AMUX_AGENT=" + defaultStr(s.Agent, "claude"),
	}
	return s.Dir, env, argv, nil
}

// agentScope returns the scope ("work"|"repo") of an agent's workgroup root, or
// "" if it can't be resolved (best-effort, for the AMUX_SCOPE hint).
func agentScope(rootID string) string {
	db, err := store.Open()
	if err != nil {
		return ""
	}
	defer db.Close()
	if root, ok, _ := db.GetSession(rootID); ok {
		return root.Scope
	}
	return ""
}

// EnsureAgentSession starts the agent's dedicated, rail-free tmux session
// (core.AgentSession) if it isn't already running, and returns its name. Used by
// the classic tmux entrypoints (`amux up`, `amux session open`); the native TUI
// no longer goes through tmux — it execs AgentCommand directly.
func EnsureAgentSession(ctx context.Context, s store.Session) (string, error) {
	sess := core.AgentSession(s.ID)
	if tmuxctl.HasSession(ctx, sess) {
		return sess, nil
	}
	dir, env, argv, err := AgentCommand(s)
	if err != nil {
		return "", err
	}
	if err := tmuxctl.NewDetachedSession(ctx, sess, dir, env, argv...); err != nil {
		return "", err
	}
	return sess, nil
}

// launch opens (or switches the attached client to) one agent's dedicated tmux
// session — used by the classic entrypoint and the CLI.
func launch(ctx context.Context, s store.Session) error {
	sess, err := EnsureAgentSession(ctx, s)
	if err != nil {
		return err
	}
	_ = tmuxctl.SwitchClient(ctx, sess)
	return nil
}

// DeleteByID deletes a session. The console can't be deleted (window closed); a
// workspace removes all its agents; an agent removes just itself.
func DeleteByID(ctx context.Context, id string) error {
	if id == console.ID {
		_ = tmuxctl.KillSession(ctx, core.AgentSession(id))
		return nil
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if s.IsRoot() {
		agents, _ := db.Children(id)
		for _, a := range agents {
			removeAgent(ctx, db, a)
		}
		// Non-recursive: remove the container dir only if empty. A re-parented agent
		// can still physically live under this root's tree (move is DB-only), so we
		// must never blow the whole tree away.
		_ = os.Remove(store.RootDir(id))
		return db.DeleteSession(id)
	}
	removeAgent(ctx, db, s)
	return nil
}

func removeAgent(ctx context.Context, db *store.DB, a store.Session) {
	_ = tmuxctl.KillSession(ctx, core.AgentSession(a.ID))
	for _, repoName := range store.SplitRepos(a.Repo) {
		if repo, ok, _ := db.Repo(repoName); ok {
			_ = git.RemoveWorktree(ctx, repo.GitDir, filepath.Join(a.Dir, repoName), a.Branch)
		}
	}
	_ = os.RemoveAll(a.Dir)
	_ = db.DeleteSession(a.ID)
}

// Apply executes a control action against the session model — the shared
// dispatch used by the daemon, the multiplexer server, and any client driver, so
// they can't drift. Transport concerns (subscribe/refresh) are no-ops here.
func Apply(ctx context.Context, a core.Action) error {
	switch a.Action {
	case "", "refresh", "subscribe":
		return nil
	case "open", "attach":
		return OpenByID(ctx, a.ID)
	case "delete", "kill":
		return DeleteByID(ctx, a.ID)
	case "move":
		return MoveAgent(ctx, a.ID, a.Target)
	case "archive":
		return ToggleArchived(ctx, a.ID)
	case "rename":
		return Rename(a.ID, a.Fields["name"])
	case "new-repo-agent":
		_, err := CreateRepoWorkgroup(ctx, a.ID, AgentSpec{
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		})
		return err
	case "new-workgroup":
		repos := store.SplitRepos(a.Fields["repos"])
		prompt := baselinePrompt(a.Fields["prompt"], a.Fields["linear"])
		var def *AgentSpec
		if len(repos) > 0 || prompt != "" {
			def = &AgentSpec{Prompt: prompt}
		}
		_, err := CreateWorkspace(ctx, a.Fields["name"], repos, def)
		return err
	}
	return fmt.Errorf("unknown action %q", a.Action)
}

// baselinePrompt weaves a Linear issue link and a description into one prompt for
// a new workgroup's first agent. MVP: the issue URL is handed to the agent in its
// prompt (no Linear API sync yet).
func baselinePrompt(description, linear string) string {
	var parts []string
	if linear = strings.TrimSpace(linear); linear != "" {
		parts = append(parts, "Linear issue to work on: "+linear)
	}
	if d := strings.TrimSpace(description); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

// SetArchived marks an agent (or workgroup) done/archived, or restores it. An
// archived session drops off the active rail but stays in the store; its tmux
// session (if any) is stopped so it isn't holding a live process.
func SetArchived(ctx context.Context, id string, archived bool) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		return fmt.Errorf("no such session %q", id)
	}
	s.Archived = archived
	if archived {
		_ = tmuxctl.KillSession(ctx, core.AgentSession(id))
	}
	return db.PutSession(s)
}

// ToggleArchived flips a session's archived flag (the native TUI's one-key mark).
func ToggleArchived(ctx context.Context, id string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	cur := false
	if s, ok, _ := db.GetSession(id); ok {
		cur = s.Archived
	}
	db.Close()
	return SetArchived(ctx, id, !cur)
}

// Rename sets a session's display name.
func Rename(id, name string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		return fmt.Errorf("no such session %q", id)
	}
	s.Name = strings.TrimSpace(name)
	return db.PutSession(s)
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
