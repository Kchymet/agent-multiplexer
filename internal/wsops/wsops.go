// Package wsops holds session lifecycle operations shared by the daemon (rail
// actions) and the CLI. A workgroup (root) is a pure container: it checks out
// nothing itself; its agents (subs) each work on a subset of the tracked repos,
// one worktree per repo under the agent's own directory.
package wsops

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/git"
	"amux/internal/skills"
	"amux/internal/store"
)

// AgentSpec describes an agent to create under a workgroup.
type AgentSpec struct {
	Repos  []string // the tracked repos this agent works on, one worktree each
	Agent  string   // defaults to "claude"
	Model  string   // optional model override
	Mode   string   // task | interactive (defaults to task)
	Prompt string   // initial prompt
}

// CreateWorkspace creates a workgroup (root): a container of agents that checks
// out nothing itself and holds no repos of its own (a repo is an attribute of
// an agent, via its worktrees), but which IS a session — the workgroup's
// coordinator (store.RoleCoordinator), sandboxed to the container dir that
// holds every member's sandbox, with a conversation pinned now so it resumes
// durably. When defaultAgent is non-nil it also creates one agent from that spec
// (its repos, model, mode, and prompt are honored). Pass nil to create an empty
// workgroup. Returns the workgroup id.
func CreateWorkspace(ctx context.Context, name string, defaultAgent *AgentSpec) (string, error) {
	return createWorkspace(ctx, name, agent.DefaultKind(), "", defaultAgent)
}

// createWorkspace is the action-path variant of CreateWorkspace. It lets a
// remote client choose the coordinator harness and model while preserving the
// public helper's long-standing Claude-default behavior for local callers.
func createWorkspace(ctx context.Context, name, kind, model string, defaultAgent *AgentSpec) (string, error) {
	if !agent.Known(kind) {
		return "", fmt.Errorf("unknown agent kind %q\n  known kinds: %s", kind, strings.Join(agent.Kinds(), ", "))
	}
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()

	rootID := db.NewID()
	kind = agent.Canonical(kind)
	root := store.Session{
		ID: rootID, RootID: "", Name: strings.TrimSpace(name), Scope: store.ScopeWork,
		Agent: kind, Model: model, Mode: store.ModeInteractive,
		Dir: store.RootDir(rootID), ClaudeID: agent.HarnessFor(kind).NewSessionID(),
		Created: store.Now(),
	}
	if err := os.MkdirAll(root.Dir, 0o755); err != nil {
		return "", err
	}
	if err := db.PutSession(root); err != nil {
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
		return store.Session{}, fmt.Errorf("unknown repo %q\n  %s", repo, trackedRepos(db))
	}
	rootID := db.NewID()
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Scope: store.ScopeRepo,
		Mode: store.NormalizeMode(spec.Mode), Repo: repo, Created: store.Now(),
	}); err != nil {
		return store.Session{}, err
	}
	spec.Repos = []string{repo}
	return addAgent(ctx, db, rootID, spec)
}

// AddAgent adds an agent (sub-session) to a workgroup.
func AddAgent(ctx context.Context, rootID string, spec AgentSpec) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok || !root.IsRoot() {
		return store.Session{}, fmt.Errorf("no such workgroup %q\n  `amux workgroup ls` lists them", rootID)
	}
	return addAgent(ctx, db, rootID, spec)
}

func addAgent(ctx context.Context, db *store.DB, rootID string, spec AgentSpec) (store.Session, error) {
	// Reject unknown harness kinds before anything is persisted: no action can
	// edit a session's kind after creation, so a typo'd --agent would otherwise
	// mint a session that errors on every launch and can only be deleted.
	if !agent.Known(spec.Agent) {
		return store.Session{}, fmt.Errorf("unknown agent kind %q\n  known kinds: %s", spec.Agent, strings.Join(agent.Kinds(), ", "))
	}
	agentID := db.NewID()
	dir := store.AgentDir(rootID, agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Session{}, err
	}
	// The branch scheme lives in one place (core.BranchFor); everything else — the
	// worktree, the agent guide, the doctor reconciliation — derives it from there.
	branch := core.BranchFor(rootID, agentID)
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
	kind := agent.Canonical(spec.Agent)
	// A harness that accepts a pre-minted conversation id (Claude) gets one pinned
	// now for durable resume across restarts. One that mints its own uuid on first
	// run (Codex) returns "" and adopts the real id afterward — see PlanLaunch.
	claudeID := agent.HarnessFor(kind).NewSessionID()
	a := store.Session{
		ID: agentID, RootID: rootID,
		Agent: kind, Model: spec.Model,
		Mode: store.NormalizeMode(spec.Mode),
		Repo: store.JoinRepos(repos), Branch: branch, Dir: dir,
		ClaudeID: claudeID, Prompt: spec.Prompt, Created: store.Now(),
	}
	// Write the guide from the session record so it's correct immediately; every
	// launch rewrites it from the current record (see AgentCommand).
	writeAgentGuide(a)
	// Seed the agent's private harness config from the user's (the template) now,
	// so what the agent starts with is what the user had at creation — not
	// whatever the template holds by the time it first launches.
	ensureConfigHome(a)
	if err := db.PutSession(a); err != nil {
		return store.Session{}, err
	}
	return a, nil
}

// ensureConfigHome seeds the agent's private harness configuration — a copy of
// the user's own config dir, placed under the agent's dir and pointed at via the
// harness's config env (see agent.Harness.Config) — if it isn't there yet. It is
// idempotent, so it runs at creation and again at every launch: an agent created
// before config homes were private gets one on its next launch, and an agent's
// own edits to its copy are never touched. Best-effort: a failure is logged and
// the launch proceeds (the harness then falls back to its default, the user's
// home — visible in the daemon log rather than blocking the agent).
func ensureConfigHome(s store.Session) {
	spec, ok := agent.HarnessFor(s.Agent).Config(s)
	if !ok {
		return
	}
	fresh, err := cfghome.Seed(spec)
	if err != nil {
		log.Printf("amux: seeding %s config for agent %s: %v", spec.Kind, s.ID, err)
		return
	}
	if fresh {
		log.Printf("amux: seeded agent %s's private %s config from %s", s.ID, spec.Kind, spec.Template)
	}
	// A legacy agent whose dir is itself a worktree must not see its config home
	// as untracked files.
	if git.IsGitRepo(context.Background(), s.Dir) {
		_ = git.Exclude(context.Background(), s.Dir, ".amux/")
	}
}

// AgentIDsUnder returns the agent (sub-session) ids to run for id: if id is a
// workgroup root, all its agent children; otherwise id itself. It lets a caller
// start the process(es) for a freshly-created session — root or agent — the same
// way the TUI starts an agent when it's opened.
func AgentIDsUnder(id string) ([]string, error) {
	// The console and a repo home are single sessions (the console has no store
	// row; a home is a root with no members), so each resolves to itself: `start`
	// and a `prompt` to a stopped one both come through here. A workgroup root
	// still resolves to its member agents — "start the workgroup" brings the
	// workers up; its coordinator starts at creation, when opened, or prompted (see
	// daemon.startForSteer, which starts exactly the steered session).
	s, ok, err := ResolveSession(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no such workgroup or agent %q\n  `amux workgroup ls` lists them", id)
	}
	if !s.IsRoot() || s.Role() == store.RoleConsole || s.Role() == store.RoleRepo {
		return []string{s.ID}, nil
	}
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
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
			wt := filepath.Join(a.Dir, r)
			_ = git.RemoveWorktree(ctx, repo.GitDir, wt, agentBranch(ctx, a, wt))
		}
	}
	// Field-scoped write: only the repo column changes here.
	return db.SetRepoScope(a.ID, store.JoinRepos(final))
}

// MoveAgent re-parents an agent into another workgroup. With an empty targetRootID
// it creates a new work-scoped workgroup seeded with the agent's repos. Only the
// agent's root_id changes — its worktree dir and branch are left where they are
// (they embed the old root id but are read from the stored values, so they keep
// working; physically moving git worktrees is avoided). An old single-member
// repo-scoped workgroup left empty is dropped (its container dir is left on disk
// because the moved agent's worktree still lives under it and is in use); a
// work-scoped one left empty stays, since it is its coordinator's session.
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

	// Field-scoped write: only the parent (root_id) changes when re-parenting.
	if err := db.SetRootID(a.ID, targetRootID); err != nil {
		return err
	}
	// Drop the old workgroup if it was the hidden single-member repo-scoped
	// wrapper around this agent, now empty. A work-scoped workgroup stays: it is
	// its coordinator's session, possibly running, and the user deletes it
	// deliberately.
	if old, ok, _ := db.GetSession(oldRoot); ok && old.Scope == store.ScopeRepo && old.Role() != store.RoleRepo {
		if kids, _ := db.Children(oldRoot); len(kids) == 0 {
			_ = db.DeleteSession(oldRoot)
		}
	}
	return nil
}

// AgentCommand resolves everything needed to run an agent: its working dir, the
// extra environment (KEY=VALUE) to set, and the argv. It decides resume vs
// continue vs fresh the same way regardless of how the caller runs it — the
// daemon's engine execs this in a PTY-backed process the native TUI attaches to.
//
// The Claude agent always launches in the agent's own root dir (s.Dir) — where
// amux installs its .claude config and writes CLAUDE.md — so Claude loads them
// (settings.local.json is read only from the launch dir, never a parent). This
// holds even for a single-repo agent, whose repo is a worktree subdir under the
// root; only the editor and terminal panes drop into that subdir (see
// AgentWorkdir). Resuming a legacy conversation is the one exception: the launch
// dir moves to wherever its transcript already lives (see resumeCwds).
func AgentCommand(s store.Session) (dir string, env, argv []string, err error) {
	dir = s.Dir
	if _, err := os.Stat(dir); err != nil {
		return "", nil, nil, fmt.Errorf("agent dir missing: %s", dir)
	}

	// Regenerate the guide from the current session record (and, for a container
	// session, the live inventory) before each launch, so its branch, repo list,
	// and roster reflect the latest state — an LLM agent that reloads its guide
	// never obeys stale instructions.
	writeGuide(s)

	h := agent.HarnessFor(s.Agent)
	prompt := strings.TrimSpace(s.Prompt)
	// The agent's private harness config must exist before anything below reads
	// or writes it (gap-fill, resume detection, trust all live in that home).
	ensureConfigHome(s)
	// Before deciding resume-vs-fresh, gap-fill the harness transcript from amux's
	// captured backup: a mid-turn kill can leave the harness's own copy missing
	// even though we hooked a backup, so restore it into the primary resume cwd
	// where resume detection looks, turning what would be a fresh start back into a
	// resume. Best-effort — RestoreTranscript no-ops when there's nothing better to
	// restore and never clobbers a fresher copy, so a failure never blocks launch.
	if s.ClaudeID != "" {
		if restored, _ := h.RestoreTranscript(s, dir); restored {
			log.Printf("amux: gap-filled transcript for %s from captured backup", s.ClaudeID)
		}
	}
	// The kind-specific resume-vs-continue-vs-fresh decision, which may relocate the
	// launch dir to wherever a transcript already lives. resumeCwds lists the cwds a
	// transcript for this agent could live under (amux's workdir convention has
	// shifted over time), preferred-first.
	plan := h.PlanLaunch(agent.LaunchRequest{Session: s, Dir: dir, Prompt: prompt, ResumeCwds: resumeCwds(s)})
	dir = plan.Dir

	// Pre-launch filesystem side effects the harness needs in its launch dir
	// (trusting the folder, installing amux's hooks). Best-effort by contract.
	h.PrepareLaunch(s, dir)

	// Install amux's built-in skill library (the PR playbook, etc.) so it tracks
	// the running binary. Where it goes is the harness's call — Claude reads
	// .claude/skills, others .agents/skills. Best-effort: a failure just means the
	// agent lacks the skills, never that it can't launch. The launch dir is normally
	// the agent's own root dir (not a git repo); if resuming into a worktree, git-exclude
	// the tree so it never dirties the repo.
	skillsDir := h.SkillsDir(dir)
	if err := skills.Install(skillsDir); err == nil && git.IsGitRepo(context.Background(), dir) {
		if rel, err := filepath.Rel(dir, skillsDir); err == nil {
			_ = git.Exclude(context.Background(), dir, rel+"/")
		}
	}
	argv, err = h.Argv(s.Model, plan.Extra...)
	if err != nil {
		return "", nil, nil, err
	}
	return dir, AgentEnv(s), argv, nil
}

// AgentEnv is the extra environment (KEY=VALUE) every pane of an agent gets —
// the exported amux intent (see the README's philosophy table), plus the
// harness's config env (CLAUDE_CONFIG_DIR / CODEX_HOME) pointing at the agent's
// private config copy, so the agent pane and a `claude`/`codex` run from the
// terminal tab both use it. It is separate from AgentCommand so the editor and
// terminal panes can be resolved without running the agent-launch side effects
// (resume decisions, trust, hook installs).
func AgentEnv(s store.Session) []string {
	env := []string{
		"AMUX_WORKGROUP=" + s.ID,
		"AMUX_WORKSPACE=" + s.ID, // back-compat alias for AMUX_WORKGROUP
		"AMUX_ROOT=" + s.RootID,
		"AMUX_SCOPE=" + ScopeOf(s),
		// The session's role: empty for an ordinary agent; console | coordinator
		// | repo for the built-in default sessions (store.Role*).
		"AMUX_ROLE=" + s.Role(),
		"AMUX_MODE=" + store.NormalizeMode(s.Mode),
		"AMUX_AGENT=" + agent.Canonical(s.Agent),
		// The harness session id, so a harness with no hook stream (unlike Claude,
		// which gets its id from the hook payload on stdin) can still self-report via
		// `amux agent status`, which falls back to $AMUX_SESSION_ID. Empty until a
		// self-minting harness (Codex) has adopted its id — matching what's pinned.
		"AMUX_SESSION_ID=" + s.ClaudeID,
	}
	if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
		env = append(env, spec.EnvEntry())
	}
	return env
}

// AgentWorkdir is the directory the editor and terminal panes run in. An agent
// dir holds one git-worktree subdir per assigned repo, so with a single repo we
// drop the human straight into that worktree (one level deeper) instead of a
// wrapper dir whose only content is that one subdir. With several (or zero) repos
// there's no single worktree to pick, so we stay at the agent dir, which holds
// them all side by side.
//
// The Claude agent pane is the exception: it always launches in the agent's own
// root dir (s.Dir), where amux installs its .claude config and CLAUDE.md, so Claude
// loads them (settings.local.json is read only from the launch dir). See
// AgentCommand.
func AgentWorkdir(s store.Session) string {
	// A container session (console, coordinator, repo home) has no worktree: its
	// Repo names the repo it is the home of (or, for a root, the union its members
	// work on), not a checkout under its dir.
	if s.IsContainerSession() {
		return s.Dir
	}
	if repos := store.SplitRepos(s.Repo); len(repos) == 1 {
		return filepath.Join(s.Dir, repos[0])
	}
	return s.Dir
}

// resumeCwds lists the working directories a Claude transcript for this agent
// could live under, so resume detection isn't fooled by amux having changed its
// workdir convention over time. The current launch dir — the agent's own root
// dir (s.Dir) — comes first, preferred on a tie; then the per-repo worktree that
// single-repo agents launched in under the older convention.
func resumeCwds(s store.Session) []string {
	cwds := []string{s.Dir}
	if wd := AgentWorkdir(s); wd != s.Dir {
		cwds = append(cwds, wd)
	}
	return cwds
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
// workgroup removes all its agents; an agent removes just itself. The daemon
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
		return fmt.Errorf("no such workgroup or agent %q\n  `amux workgroup ls` lists them", id)
	}
	if s.IsRoot() {
		if s.Role() == store.RoleRepo {
			return fmt.Errorf("%q is repo %s's home session; it goes with the repo (amux repo rm %s)", id, s.Repo, s.Repo)
		}
		agents, _ := db.Children(id)
		for _, a := range agents {
			removeAgent(ctx, db, a)
		}
		// The coordinator's own files go; the container dir itself is removed only
		// if that leaves it empty. A re-parented agent can still physically live
		// under this root's tree (move is DB-only), so we must never blow the whole
		// tree away.
		removeContainerFiles(db, s)
		return db.DeleteSession(id)
	}
	removeAgent(ctx, db, s)
	return nil
}

func removeAgent(ctx context.Context, db *store.DB, a store.Session) {
	for _, repoName := range store.SplitRepos(a.Repo) {
		if repo, ok, _ := db.Repo(repoName); ok {
			wt := filepath.Join(a.Dir, repoName)
			_ = git.RemoveWorktree(ctx, repo.GitDir, wt, agentBranch(ctx, a, wt))
		}
	}
	// The private config home goes with the dir; drop its seed manifest too.
	if spec, ok := agent.HarnessFor(a.Agent).Config(a); ok {
		cfghome.Forget(spec)
	}
	_ = os.RemoveAll(a.Dir)
	_ = db.DeleteSession(a.ID)
}

// agentBranch is the branch to delete along with agent a's worktree at wt, or ""
// for none. The stored branch is authoritative. A session imported from the
// legacy workspaces/ layout has none (importLegacy never set one), which used to
// make removal skip the branch delete entirely and leave amux/<root> behind in
// every repo — exactly the "branch with no agent" leftovers doctor then reports.
// So with a blank record, ask the worktree what it has checked out, and if the
// worktree is already gone fall back to the legacy scheme. A derived name is only
// trusted if it's amux-owned: a worktree someone switched onto main is never a
// reason to delete main.
func agentBranch(ctx context.Context, a store.Session, wt string) string {
	if a.Branch != "" {
		return a.Branch
	}
	b := git.CurrentBranch(ctx, wt)
	if b == "" {
		b = core.LegacyBranchFor(a.RootID)
	}
	if !strings.HasPrefix(b, core.BranchPrefix) {
		return ""
	}
	return b
}

// Apply executes a control action against the session model — the shared
// dispatch used by the daemon, the multiplexer server, and any client driver, so
// they can't drift. Transport concerns (subscribe/refresh) are no-ops here.
func Apply(ctx context.Context, a core.Action) error {
	_, err := ApplyResult(ctx, a)
	return err
}

// Dispatch is THE control-action path: it stops the target's running process(es)
// via stopEngine when the verb's descriptor calls for it, then applies the store
// mutation, returning any created session id. The daemon passes its engine-kill,
// the mux server its pane-teardown, a backend-less driver passes nil — so all
// three run every verb through one place and can't drift. (The archive vs
// set-archived engine-stop split that motivated this lived in exactly that gap.)
// stopEngine runs before ApplyResult so it can read the still-present snapshot —
// e.g. a root delete cascades to children that ApplyResult is about to remove.
func Dispatch(ctx context.Context, a core.Action, stopEngine func(id string)) (string, error) {
	if stopEngine != nil && core.DescriptorFor(a.Action).StopsEngine {
		stopEngine(a.ID)
	}
	return ApplyResult(ctx, a)
}

// ApplyResult is Apply plus the id of any session the action created: the new
// agent for new-repo-agent/add-agent, or the workgroup root for new-workgroup.
// It lets a caller switch to (and thereby actually start) the agent it just
// created instead of leaving it initialized-but-not-running. The id is "" for
// actions that create nothing.
func ApplyResult(ctx context.Context, a core.Action) (string, error) {
	switch a.Action {
	case "", core.ActionRefresh, core.ActionSubscribe:
		return "", nil
	case core.ActionDelete, core.ActionKill:
		return "", DeleteByID(ctx, a.ID)
	case core.ActionMove:
		return "", MoveAgent(ctx, a.ID, a.Target)
	case core.ActionArchive:
		return "", ToggleArchived(ctx, a.ID)
	case core.ActionSetArchived:
		// Explicit archive/unarchive (the CLI's `archive`/`unarchive`), vs the
		// TUI's one-key "archive" toggle above.
		return "", SetArchived(ctx, a.ID, a.Fields["archived"] == "true")
	case core.ActionRename:
		return "", Rename(a.ID, a.Fields["name"])
	case core.ActionRmRepo:
		return "", RemoveRepo(a.ID)
	case core.ActionAgentSetRepos:
		// Re-scope an existing agent to exactly the given repos (fuzzy-picked),
		// adding/removing worktrees to match. This is the "pull a repo into scope"
		// action.
		return "", SetAgentRepos(ctx, a.ID, store.SplitRepos(a.Fields["repos"]))
	case core.ActionNewRepoAgent:
		s, err := CreateRepoWorkgroup(ctx, a.ID, AgentSpec{
			Agent:  agentOf(a.Fields),
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		})
		return s.ID, err
	case core.ActionAddAgent:
		// Repos come straight from the fuzzy picker; zero is allowed (repo-less
		// agent). A workgroup no longer carries repos of its own to fall back to.
		s, err := AddAgent(ctx, a.ID, AgentSpec{
			Agent:  agentOf(a.Fields),
			Repos:  store.SplitRepos(a.Fields["repos"]),
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		})
		return s.ID, err
	case core.ActionAddRepo:
		_, err := AddRepoSource(ctx, a.Fields["source"])
		return "", err
	case core.ActionNewWorkgroup:
		prompt := baselinePrompt(a.Fields["prompt"], a.Fields["linear"])
		repos := store.SplitRepos(a.Fields["repos"])
		var def *AgentSpec
		if len(repos) > 0 || prompt != "" {
			def = &AgentSpec{Agent: agentOf(a.Fields), Repos: repos, Mode: a.Fields["mode"], Model: a.Fields["model"], Prompt: prompt}
		}
		// Return the workgroup root; the client resolves it to the first agent.
		return createWorkspace(ctx, a.Fields["name"], agentOf(a.Fields), a.Fields["model"], def)
	case core.ActionCreateWorkspace:
		// The CLI's `session create`/`new`: create a workgroup, optionally seeding
		// one default agent (Fields["defaultAgent"]=="1") scoped to the given repos
		// with an explicit mode/model/prompt. When the interactive flow configures
		// its own agents it passes defaultAgent="" and follows up with add-agent.
		var def *AgentSpec
		if a.Fields["defaultAgent"] == "1" {
			def = &AgentSpec{
				Agent: agentOf(a.Fields), Repos: store.SplitRepos(a.Fields["repos"]),
				Mode: a.Fields["mode"], Model: a.Fields["model"], Prompt: a.Fields["prompt"],
			}
		}
		return createWorkspace(ctx, a.Fields["name"], agentOf(a.Fields), a.Fields["model"], def)
	}
	// A verb that reaches here is one no dispatch path claims. The CLI screens
	// these before they leave the machine, so this is the answer for anything
	// else on the socket — teach the vocabulary rather than just rejecting it.
	return "", fmt.Errorf("unknown action %q\n  valid actions: %s",
		a.Action, strings.Join(core.ControlActions(), ", "))
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
		return fmt.Errorf("no such workgroup or agent %q\n  `amux workgroup ls` lists them", id)
	}
	// Stamp the archive time so the rail can order the ARCHIVED section by when
	// agents were archived. Only stamp on a false→true transition; re-archiving
	// an already-archived session leaves its original time intact. Clear on
	// restore so a later re-archive gets a fresh timestamp.
	archivedAt := s.ArchivedAt
	switch {
	case archived && !s.Archived:
		archivedAt = store.Now()
	case !archived:
		archivedAt = 0
	}
	// Field-scoped write: only the archived flag + timestamp, so a launch adopting a
	// codex id (or a concurrent rename) isn't reverted by a full-row upsert.
	return db.SetArchivedFlag(id, archived, archivedAt)
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
	if _, ok, err := db.GetSession(id); err != nil || !ok {
		return fmt.Errorf("no such workgroup or agent %q\n  `amux workgroup ls` lists them", id)
	}
	// Field-scoped write: only the name column, so a rename can't clobber a
	// concurrent claude-id adoption or archive of the same row.
	return db.SetName(id, strings.TrimSpace(name))
}

// agentOf resolves an action's requested harness from its fields, canonicalizing
// an absent field to the default kind — older clients (and actions that predate
// the harness selector) send no "agent", and their agents must stay the default.
func agentOf(fields map[string]string) string {
	return agent.Canonical(fields["agent"])
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
