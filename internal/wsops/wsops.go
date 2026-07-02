// Package wsops holds session lifecycle operations shared by the daemon (rail
// actions) and the CLI. A workspace (root) is a template that attaches repos but
// checks out nothing itself; its agents (subs) each work on a subset of those
// repos, one worktree per repo under the agent's own directory.
package wsops

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/git"
	"amux/internal/skills"
	"amux/internal/store"
)

// AgentSpec describes an agent to create under a workspace.
type AgentSpec struct {
	Repos  []string // subset of the workspace's repos this agent works on
	Agent  string   // defaults to "claude"
	Model  string   // optional model override
	Mode   string   // task | loop (defaults to task)
	Prompt string   // initial prompt
}

// CreateWorkspace creates a workgroup (root) — a pure container of agents that
// checks out nothing itself and holds no repos of its own (a repo is an attribute
// of an agent, via its worktrees). When defaultAgent is non-nil it also creates
// one agent from that spec (its repos, model, mode, and prompt are honored). Pass
// nil to create an empty workgroup. Returns the workgroup id.
func CreateWorkspace(ctx context.Context, name string, defaultAgent *AgentSpec) (string, error) {
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()

	rootID := db.NewID()
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Name: strings.TrimSpace(name), Scope: store.ScopeWork,
		Mode: store.ModeTask, Created: store.Now(),
	}); err != nil {
		return "", err
	}
	if defaultAgent != nil {
		if _, err := addAgent(ctx, db, rootID, *defaultAgent); err != nil {
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
	// Repos are chosen from tracked ones (the fuzzy picker), but be defensive: a
	// stale or mistyped name shouldn't abort the whole create — skip it with a
	// warning and carry on with the repos that do resolve. An agent with zero
	// repos is valid (it runs in its own sandbox dir; see AgentWorkdir).
	var repos []string
	for _, repoName := range spec.Repos {
		repo, ok, err := db.Repo(repoName)
		if err != nil || !ok {
			log.Printf("amux: skipping unknown repo %q while creating agent under %s", repoName, rootID)
			continue
		}
		if err := git.AddWorktree(ctx, repo.GitDir, filepath.Join(dir, repoName), branch); err != nil {
			return store.Session{}, err
		}
		repos = append(repos, repoName)
	}
	writeAgentGuide(dir, branch, spec.Agent)
	a := store.Session{
		ID: agentID, RootID: rootID,
		Agent: defaultStr(spec.Agent, "claude"), Model: spec.Model,
		Mode: defaultStr(spec.Mode, store.ModeTask),
		Repo: store.JoinRepos(repos), Branch: branch, Dir: dir,
		ClaudeID: store.NewUUID(), Prompt: spec.Prompt, Created: store.Now(),
	}
	if err := db.PutSession(a); err != nil {
		return store.Session{}, err
	}
	return a, nil
}

// writeAgentGuide drops the sandbox guide into the agent's directory (its cwd),
// telling the agent to stay sandboxed to this dir and to keep its branch current
// with the remote. It's written to the file the agent's provider actually reads —
// CLAUDE.md for Claude Code, AGENTS.md for others (see agent.Harness.GuideFile) —
// so each CLI loads it natively. The agent dir is not a git repo, so this never
// dirties a worktree.
func writeAgentGuide(dir, branch, agentKind string) {
	guide := fmt.Sprintf(`# amux agent — sandboxed workspace

This directory is your sandbox. It contains a git **worktree per repository** you
are assigned (the subdirectories here).

## Stay in your sandbox
- Keep all **edits** inside this directory (your worktrees). Do not write outside
  it: other agents' worktrees, the amux data dir, or any parent/clone of these
  repos. (Reading the shared Claude sessions below is the one exception.)
- You are on branch `+"`%s`"+`. Commit only to this branch. Do not switch to or
  commit on the default branch (main/master), and do not push to it.

## Reason across Claude sessions
You can **read** the transcripts of every Claude Code session on this machine —
your own, other agents', and the user's — to reason about work that spans
conversations: recurring tasks, prior decisions, and what's already been done.
List them (most recent first) with:

    amux agent sessions

Each row is a session; the indented line is the transcript path (a JSONL
conversation log) you can open with your normal file tools. Add `+"`--json`"+` for
machine-readable records. This is read-only context — never modify these files,
and keep every edit inside your worktree.

## Keep current with the remote
Each repo here is a worktree of a shared clone of its remote. Before starting,
and regularly as you work, refresh your branch from the remote — run inside each
repo subdirectory:

    git fetch origin && git rebase origin/HEAD

Resolve conflicts on your branch. This keeps you building on the latest remote,
not a stale snapshot.

## Shipping your work
When a change is ready for review, use the `+"`create-pr`"+` skill — it encodes this
project's end-to-end PR flow (commit and push conventions, opening the PR, then
babysitting it: watching CI, weighing review feedback on its merits, and
resolving conflicts) so you take a change all the way to merged, not just opened.
`, branch)
	_ = os.WriteFile(agent.HarnessFor(agentKind).GuideFile(dir), []byte(guide), 0o644)
}

// AgentIDsUnder returns the agent (sub-session) ids to run for id: if id is a
// workgroup root, all its agent children; otherwise id itself. It lets a caller
// start the process(es) for a freshly-created session — root or agent — the same
// way the TUI starts an agent when it's opened.
func AgentIDsUnder(id string) ([]string, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no such session %q", id)
	}
	if !s.IsRoot() {
		return []string{id}, nil
	}
	kids, err := db.Children(id)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(kids))
	for _, k := range kids {
		ids = append(ids, k.ID)
	}
	return ids, nil
}

// SetAgentRepos changes an agent's repo scope to exactly want: it adds a worktree
// for each newly-scoped tracked repo and removes the worktree of each dropped one,
// then records the new set. Untracked names in want are skipped with a warning
// (never fatal). This is how a repo is pulled into (or out of) an existing agent's
// scope after it's created — repos are a dynamic attribute of the agent.
func SetAgentRepos(ctx context.Context, agentID string, want []string) error {
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

	cur := map[string]bool{}
	for _, r := range store.SplitRepos(a.Repo) {
		cur[r] = true
	}
	wantSet := map[string]bool{}
	var final []string
	for _, r := range want {
		if wantSet[r] {
			continue
		}
		wantSet[r] = true
		repo, ok, _ := db.Repo(r)
		if !ok {
			log.Printf("amux: skipping unknown repo %q while re-scoping agent %s", r, agentID)
			continue
		}
		if !cur[r] {
			if err := git.AddWorktree(ctx, repo.GitDir, filepath.Join(a.Dir, r), a.Branch); err != nil {
				return err
			}
		}
		final = append(final, r)
	}
	// Remove worktrees for repos no longer in scope.
	for r := range cur {
		if wantSet[r] {
			continue
		}
		if repo, ok, _ := db.Repo(r); ok {
			_ = git.RemoveWorktree(ctx, repo.GitDir, filepath.Join(a.Dir, r), a.Branch)
		}
	}
	a.Repo = store.JoinRepos(final)
	return db.PutSession(a)
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
		newRoot, err := CreateWorkspace(ctx, defaultStr(a.Name, a.Repo), nil)
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
	// Drop the old workgroup if it's now empty (always true for a moved-out
	// single-member repo-scoped one).
	if kids, _ := db.Children(oldRoot); len(kids) == 0 {
		_ = db.DeleteSession(oldRoot)
	}
	return nil
}

// AgentCommand resolves everything needed to run an agent: its working dir, the
// extra environment (KEY=VALUE) to set, and the argv. It decides resume vs
// continue vs fresh the same way regardless of how the caller runs it — the
// daemon's engine execs this in a PTY-backed process the native TUI attaches to.
//
// The Claude agent always launches in the workspace root (s.Dir) — the dir where
// amux installs its .claude config and writes CLAUDE.md — so Claude loads them
// (settings.local.json is read only from the launch dir, never a parent). This
// holds even for a single-repo agent, whose repo is a worktree subdir under the
// root; only the editor and terminal panes drop into that subdir (see
// AgentWorkdir). Resuming a legacy conversation is the one exception: the launch
// dir moves to wherever its transcript already lives (see resumeCwds).
func AgentCommand(s store.Session) (dir string, env, argv []string, err error) {
	dir = s.Dir
	if _, err := os.Stat(dir); err != nil {
		return "", nil, nil, fmt.Errorf("session dir missing: %s", dir)
	}

	prompt := strings.TrimSpace(s.Prompt)
	var extra []string
	// Before deciding resume-vs-fresh, gap-fill the harness transcript from amux's
	// captured backup: a mid-turn kill can leave Claude's own copy missing even
	// though we hooked a backup, so restore it into the primary resume cwd where
	// FindSession looks, turning what would be a fresh start back into a resume.
	// Best-effort — RestoreTranscript no-ops when there's nothing better to restore
	// and never clobbers a fresher copy, so a failure never blocks the launch.
	if s.ClaudeID != "" {
		if restored, _ := agent.HarnessFor(s.Agent).RestoreTranscript(dir, s.ClaudeID); restored {
			log.Printf("amux: gap-filled Claude transcript for %s from captured backup", s.ClaudeID)
		}
	}
	switch {
	case s.ClaudeID != "":
		// A conversation is pinned. Its transcript may live under either working-dir
		// convention (the current launch cwd, or the agent dir used before single-repo
		// agents dropped into their worktree), so search both and, if found, resume
		// under the exact cwd it lives under — that's the one Claude's own path munge
		// will match. Only `--resume` when the transcript is really there; `--session-id`
		// errors if the id is already known to Claude.
		if cwd, ok := claudecfg.FindSession(s.ClaudeID, resumeCwds(s)...); ok {
			dir = cwd
			extra = []string{"--resume", s.ClaudeID}
			core.ClearNotice(s.ClaudeID)
		} else {
			// Pinned but no transcript under any candidate path: don't silently start
			// fresh — make the fallback visible in the log and on the rail.
			warnResumeFailed(s)
			extra = []string{"--session-id", s.ClaudeID}
			if prompt != "" {
				extra = append(extra, prompt)
			}
		}
	case claudecfg.AnySession(dir):
		extra = []string{"--continue"}
	default:
		if prompt != "" {
			extra = []string{prompt}
		}
	}

	if s.Agent == "" || s.Agent == "claude" {
		_ = claudecfg.TrustDir(dir)
		// Install the status/capture hooks into this agent's launch dir (not the
		// user-wide settings.json), pointed at the stable installed binary. Claude
		// loads settings.local.json only from the launch dir, so dir must be the
		// cwd the pane runs in — normally the workspace root, which isn't a git repo
		// so nothing needs excluding. When resuming a legacy conversation dir can be
		// a repo worktree instead; git-exclude the file so it never dirties it.
		if err := claudecfg.InstallHooksIn(dir, core.InstalledBinPath()); err == nil {
			if git.IsGitRepo(context.Background(), dir) {
				_ = git.Exclude(context.Background(), dir, ".claude/settings.local.json")
			}
		}
	}
	// Install amux's built-in skill library (the PR playbook, etc.) so it tracks
	// the running binary. Where it goes is the provider's call — Claude reads
	// .claude/skills, others .agents/skills — so ask the harness. Best-effort: a
	// failure just means the agent lacks the skills, never that it can't launch.
	// The launch dir is normally the workspace root (not a git repo); if resuming
	// into a worktree, git-exclude the tree so it never dirties the repo.
	skillsDir := agent.HarnessFor(s.Agent).SkillsDir(dir)
	if err := skills.Install(skillsDir); err == nil && git.IsGitRepo(context.Background(), dir) {
		if rel, err := filepath.Rel(dir, skillsDir); err == nil {
			_ = git.Exclude(context.Background(), dir, rel+"/")
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
	return dir, env, argv, nil
}

// AgentWorkdir is the directory the editor and terminal panes run in. An agent
// dir holds one git-worktree subdir per assigned repo, so with a single repo we
// drop the human straight into that worktree (one level deeper) instead of a
// wrapper dir whose only content is that one subdir. With several (or zero) repos
// there's no single worktree to pick, so we stay at the agent dir, which holds
// them all side by side.
//
// The Claude agent pane is the exception: it always launches in the workspace
// root (s.Dir), where amux installs its .claude config and CLAUDE.md, so Claude
// loads them (settings.local.json is read only from the launch dir). See
// AgentCommand.
func AgentWorkdir(s store.Session) string {
	if repos := store.SplitRepos(s.Repo); len(repos) == 1 {
		return filepath.Join(s.Dir, repos[0])
	}
	return s.Dir
}

// resumeCwds lists the working directories a Claude transcript for this agent
// could live under, so resume detection isn't fooled by amux having changed its
// workdir convention over time. The current launch dir — the workspace root
// (s.Dir) — comes first, preferred on a tie; then the per-repo worktree that
// single-repo agents launched in under the older convention.
func resumeCwds(s store.Session) []string {
	cwds := []string{s.Dir}
	if wd := AgentWorkdir(s); wd != s.Dir {
		cwds = append(cwds, wd)
	}
	return cwds
}

// warnResumeFailed surfaces — in the daemon log and on the rail — that a pinned
// Claude conversation couldn't be resumed, so the user knows they've been
// dropped into a fresh session rather than continuing where they left off. The
// rail notice is keyed by the Claude id and cleared the next time the session
// resumes successfully.
func warnResumeFailed(s store.Session) {
	log.Printf("amux: pinned Claude conversation %s (agent %s) has no transcript under %v; starting a new conversation with that id",
		s.ClaudeID, s.ID, resumeCwds(s))
	_ = core.WriteNotice(s.ClaudeID, "couldn't resume pinned conversation — started fresh")
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

// DeleteByID deletes a session. The console can't be deleted (it's built in); a
// workspace removes all its agents; an agent removes just itself. The daemon
// stops any live engine instance before calling this (see killEngineFor).
func DeleteByID(ctx context.Context, id string) error {
	if id == console.ID {
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
	_, err := ApplyResult(ctx, a)
	return err
}

// ApplyResult is Apply plus the id of any session the action created: the new
// agent for new-repo-agent/add-agent, or the workgroup root for new-workgroup.
// It lets a caller switch to (and thereby actually start) the agent it just
// created instead of leaving it initialized-but-not-running. The id is "" for
// actions that create nothing.
func ApplyResult(ctx context.Context, a core.Action) (string, error) {
	switch a.Action {
	case "", "refresh", "subscribe":
		return "", nil
	case "delete", "kill":
		return "", DeleteByID(ctx, a.ID)
	case "move":
		return "", MoveAgent(ctx, a.ID, a.Target)
	case "archive":
		return "", ToggleArchived(ctx, a.ID)
	case "set-archived":
		// Explicit archive/unarchive (the CLI's `archive`/`unarchive`), vs the
		// TUI's one-key "archive" toggle above.
		return "", SetArchived(ctx, a.ID, a.Fields["archived"] == "true")
	case "rename":
		return "", Rename(a.ID, a.Fields["name"])
	case "rm-repo":
		return "", RemoveRepo(a.ID)
	case "agent-set-repos":
		// Re-scope an existing agent to exactly the given repos (fuzzy-picked),
		// adding/removing worktrees to match. This is the "pull a repo into scope"
		// action.
		return "", SetAgentRepos(ctx, a.ID, store.SplitRepos(a.Fields["repos"]))
	case "new-repo-agent":
		s, err := CreateRepoWorkgroup(ctx, a.ID, AgentSpec{
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		})
		return s.ID, err
	case "add-agent":
		// Repos come straight from the fuzzy picker; zero is allowed (repo-less
		// agent). A workgroup no longer carries repos of its own to fall back to.
		s, err := AddAgent(ctx, a.ID, AgentSpec{
			Agent:  "claude",
			Repos:  store.SplitRepos(a.Fields["repos"]),
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		})
		return s.ID, err
	case "add-repo":
		_, err := AddRepoSource(ctx, a.Fields["source"])
		return "", err
	case "new-workgroup":
		prompt := baselinePrompt(a.Fields["prompt"], a.Fields["linear"])
		repos := store.SplitRepos(a.Fields["repos"])
		var def *AgentSpec
		if len(repos) > 0 || prompt != "" {
			def = &AgentSpec{Agent: "claude", Repos: repos, Prompt: prompt}
		}
		// Return the workgroup root; the client resolves it to the first agent.
		return CreateWorkspace(ctx, a.Fields["name"], def)
	case "create-workspace":
		// The CLI's `session create`/`new`: create a workgroup, optionally seeding
		// one default agent (Fields["defaultAgent"]=="1") scoped to the given repos
		// with an explicit mode/model/prompt. When the interactive flow configures
		// its own agents it passes defaultAgent="" and follows up with add-agent.
		var def *AgentSpec
		if a.Fields["defaultAgent"] == "1" {
			def = &AgentSpec{
				Agent: "claude", Repos: store.SplitRepos(a.Fields["repos"]),
				Mode: a.Fields["mode"], Model: a.Fields["model"], Prompt: a.Fields["prompt"],
			}
		}
		return CreateWorkspace(ctx, a.Fields["name"], def)
	}
	return "", fmt.Errorf("unknown action %q", a.Action)
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
// archived session drops off the active rail but stays in the store; the daemon
// stops its engine instance (see killEngineFor) so it isn't holding a live
// process.
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
