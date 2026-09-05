package wsops

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
)

// writeGuide drops the sandbox guide into a session's directory (its cwd): the
// file the harness reads on launch — CLAUDE.md for Claude Code, AGENTS.md for
// others (agent.Harness.GuideFile). Which guide depends on the session's role:
// an ordinary agent gets the member guide (stay sandboxed, keep the branch
// current, ship via PR); the console, a workgroup coordinator, and a repo home
// each get a guide for that role, templated from the live store so the roster it
// describes is the one that exists at launch. Every guide is rewritten at every
// launch (see AgentCommand), so an LLM agent never obeys stale instructions
// after its scope, its workgroup, or the inventory changes. The dir is not a git
// repo, so this never dirties a worktree.
func writeGuide(s store.Session) {
	var guide string
	switch s.Role() {
	case store.RoleConsole:
		guide = consoleGuide(s)
	case store.RoleCoordinator:
		guide = coordinatorGuide(s)
	case store.RoleRepo:
		guide = repoGuide(s)
	default:
		guide = memberGuide(s)
	}
	_ = os.WriteFile(agent.HarnessFor(s.Agent).GuideFile(s.Dir), []byte(guide), 0o644)
}

// writeAgentGuide is writeGuide under its historical name (creation paths call
// it for a fresh agent).
func writeAgentGuide(s store.Session) { writeGuide(s) }

// memberGuide is the guide for an ordinary agent: sandboxed to its dir, on its
// own branch, shipping through the PR flow. It is templated from the session
// record — the branch (s.Branch, authoritative even after a move) and the
// assigned repos.
func memberGuide(s store.Session) string {
	repos := "This agent has no repos attached — it works in its own sandbox dir."
	if names := store.SplitRepos(s.Repo); len(names) > 0 {
		repos = "You are assigned these repos (one worktree subdir each): " + strings.Join(names, ", ") + "."
	}
	return fmt.Sprintf(`# amux agent — your sandbox

This directory is your sandbox. It contains a git **worktree per repository** you
are assigned (the subdirectories here). %s

## Stay in your sandbox
- Keep source and configuration **edits** inside this directory (your worktrees).
  Run git commands from your assigned worktree. Git may write its backing metadata
  (objects, refs, index, locks, and config) in amux's shared bare clone outside this
  directory; amux mounts that metadata writable for this purpose. This is allowed.
  Do not manually edit the shared clone, other agents' worktrees, or amux's state.
  Reading the shared agent sessions below is also allowed.
- You may commit, fetch, merge, push your assigned branch, and open or update its
  pull request with gh using the host's shared GitHub authentication. These are
  normal sandbox operations; keep the sandbox enabled.
- You are on branch `+"`%s`"+`. Commit only to this branch. Do not switch to or
  commit on the default branch (main/master), and do not push to it.

`+configHomeSection+`
`+transcriptsSection+`
## Keep current with the remote
Each repo here is a worktree of a shared clone of its remote, and other agents
may be working the same repo in parallel on their own branches. Before starting,
and regularly as you work, refresh your branch from the remote — run inside each
repo subdirectory:

    git fetch origin && git merge --no-edit origin/HEAD

**Merge, don't rebase.** Once you've pushed your branch for a PR, rebasing
rewrites commits the remote already has, so your next push is rejected as
non-fast-forward — the only way through is a force-push, which needs a human to
unblock. Merging only ever adds commits, so every push stays a fast-forward: you
can update a PR without ever force-pushing or asking for help. The project
squash-merges PRs, so the extra merge commits never reach the default branch.
Resolve any conflicts on your branch. This keeps you building on the latest
remote, not a stale snapshot.

## Shipping your work
When a change is ready for review, use the `+"`create-pr`"+` skill — it encodes this
project's end-to-end PR flow (commit and push conventions, opening the PR, then
babysitting it: watching CI, weighing review feedback on its merits, and
resolving conflicts) so you take a change all the way to merged, not just opened.
`, repos, s.Branch)
}

// Sections shared by every guide.
const (
	configHomeSection = `## Your own configuration
Your harness's configuration (Claude Code's ` + "`~/.claude`" + `, Codex's ` + "`$CODEX_HOME`" + `) is a
**private copy** under ` + "`.amux/`" + ` in this directory, seeded from the user's own config
as a template; ` + "`CLAUDE_CONFIG_DIR`" + ` / ` + "`CODEX_HOME`" + ` point at it. You may edit it —
settings, memory, skills, MCP servers — it is yours. amux compares your copy with
the template and reports what you changed, so the user can decide whether a change
should propagate to their config and to other agents (` + "`amux sandbox drift`" + `). Nothing
you change there propagates on its own. The credentials file is shared and not yours
to edit.
`
	transcriptsSection = `## Reason across agent sessions
You can **read** the transcripts of every agent session on this machine (Claude
Code, Codex, …) — your own, other agents', and the user's — to reason about work that spans
conversations: recurring tasks, prior decisions, and what's already been done.
List them (most recent first) with:

    amux agent sessions

Each row is a session; the indented line is the transcript path (a JSONL
conversation log) you can open with your normal file tools. Add ` + "`--json`" + ` for
machine-readable records. This is read-only context — never modify these files,
and keep every edit inside your own directory.
`
	// guideRegenNote tells a long-lived session where durable instructions go,
	// since its guide is rewritten at every launch.
	guideRegenNote = `
This file is regenerated from amux's live state at every launch, so edits here
are lost; durable instructions for yourself belong in your private config home's
memory (` + "`.amux/claude/CLAUDE.md`" + ` for Claude Code) or in notes you keep in this
directory.`
	// steeringVerbs is the operating vocabulary every container session shares.
	steeringVerbs = `- ` + "`amux status --json`" + ` — the live rail: every session with its state
  (idle | ready | waiting | running), title, repos, and sandbox dir.
- ` + "`amux do steer <id> -f verb=prompt -f text=\"…\"`" + ` — send an agent a prompt
  (this starts a stopped agent); ` + "`-f verb=interject`" + ` speaks mid-turn,
  ` + "`-f verb=stop`" + ` interrupts the turn, ` + "`-f verb=permission -f decision=allow|deny`" + `
  answers a pending permission prompt.
- ` + "`amux do archive <id>`" + ` marks an agent done (reversible); ` + "`amux do rename <id> -f name=\"…\"`" + `;
  ` + "`amux do start <id>`" + ` brings a stopped agent up without prompting it.`
)

// consoleGuide is the guide for the machine-wide console: context over every
// workgroup, agent, and repo the daemon runs, and the CLI to operate them.
func consoleGuide(s store.Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# amux console — machine-wide context

You are the **amux console**: the built-in session with global context over
everything amux runs on this machine. amux is an AI-native terminal control plane:
it runs coding agents (Claude Code, Codex) in sandboxed sessions, grouped into
**workgroups** (a coordinator plus agents working across repos) and **repos**
(a home session plus one-off agents on a single tracked repo), shown on a rail in
a native TUI and mirrored to a web dashboard.

## Your role
- Answer for the whole machine: which workgroups, agents, and repos exist, what
  each agent was asked to do, what state it is in, and what it has done — read
  its transcript before you say.
- Operate amux for the user: create workgroups and agents, steer, archive, and
  rename them, track repos, tune amux's configuration.
- Coordinate *across* workgroups when asked. Coordination *within* one belongs to
  that workgroup's coordinator session (open it from the rail, or steer it by its
  workgroup id); context on one repo's one-off agents belongs to that repo's home
  session (its id is the repo name).

## Where everything is
- This directory (%s) is your sandbox: the only place you write. %s
- amux's data dir (%s), readable, holds:
  - `+"`amux.db`"+` — the SQLite store of repos and sessions (read it via the CLI, never edit it);
  - `+"`repos/<name>.git`"+` — bare clones every agent worktree is sourced from;
  - `+"`sessions/<workgroup>/`"+` — a workgroup's container: the coordinator's sandbox,
    holding `+"`<agent>/`"+` sandboxes with each agent's worktrees, its own `+"`CLAUDE.md`"+`,
    and its private config (and transcript) under `+"`.amux/`"+`;
  - `+"`sessions/<repo>/`"+` — a repo home's sandbox.
- Every agent's conversation: `+"`amux agent sessions`"+` lists them with transcript paths.

## Operate amux
%s
- `+"`amux do new-workgroup -f name=\"…\" -f repos=a,b -f prompt=\"…\"`"+` creates a
  workgroup with a first agent; `+"`amux do add-agent <workgroup> -f repos=… -f prompt=\"…\"`"+`
  adds one (`+"`-f agent=claude|codex -f model=… -f mode=task|interactive`"+` are optional);
  `+"`amux do new-repo-agent <repo> -f prompt=\"…\"`"+` starts a one-off agent on a repo;
  `+"`amux do move <agent> --target <workgroup>`"+` re-parents an agent.
- `+"`amux repo add <OWNER/REPO|url|path>`"+`, `+"`amux repo ls`"+`, `+"`amux workgroup ls`"+`.
- `+"`amux sandbox drift`"+` reviews config edits agents made; `+"`amux doctor`"+` checks health;
  `+"`amux config`"+` shows and changes amux settings.

## Rules
- Never edit a worktree, another session's sandbox, a bare clone, or the store.
  Change amux state through the CLI; put code changes in an agent, not here.
- Verify before you report: an agent's transcript, its branch, and its PR are
  the evidence — not its last status word.

`, s.Dir, guideRegenNote, core.DataDir(), steeringVerbs)
	b.WriteString(configHomeSection)
	b.WriteString("\n")
	b.WriteString(inventorySection())
	return b.String()
}

// coordinatorGuide is the guide for a workgroup root's own session: the
// coordinator of that workgroup's agents, working in the container dir that
// holds every member's sandbox.
func coordinatorGuide(root store.Session) string {
	var b strings.Builder
	name := root.Display()
	fmt.Fprintf(&b, `# amux workgroup coordinator — %s

You are the **coordinator** of workgroup **%s** (id `+"`%s`"+`): a long-lived session
that supervises this workgroup's agents. You do not implement changes yourself —
you scope the work, dispatch and steer agents, verify what they produce against
evidence, and keep the user informed. Prompting this workgroup from the rail or
the web reaches you.

## Your sandbox
This directory (%s) is the workgroup's container and your writable sandbox. Each
member agent's sandbox is a subdirectory of it (`+"`<agent id>/`"+`), holding that
agent's worktrees (one per assigned repo), its own `+"`CLAUDE.md`"+`, and its private
config home under `+"`.amux/`"+` — where its transcript lives. Read any of it
freely; **never edit an agent's worktree** — steer the agent instead. Keep your
own notes here (a `+"`COORDINATION.md`"+` with the roster, decisions, and acceptance
criteria is the record that survives your context). %s

## Operate this workgroup
%s
- `+"`amux do add-agent %s -f repos=… -f prompt=\"…\"`"+` adds an agent to this
  workgroup (`+"`-f agent=claude|codex -f model=… -f mode=task|interactive`"+` are optional).
- Every agent commits on its own branch (`+"`amux/%s-<agent>`"+`) and ships through a
  pull request; review the PR and the transcript, not the agent's summary of them.

`, name, name, root.ID, root.Dir, guideRegenNote, steeringVerbs, root.ID, root.ID)
	b.WriteString(membersSection(root))
	b.WriteString("\n")
	b.WriteString(configHomeSection)
	b.WriteString("\n")
	b.WriteString(transcriptsSection)
	return b.String()
}

// repoGuide is the guide for a tracked repo's home session: the long-lived
// context for the one-off agents run against that repo.
func repoGuide(home store.Session) string {
	var b strings.Builder
	repoName := home.Repo
	gitDir, source := "", ""
	if db, err := store.Open(); err == nil {
		if r, ok, _ := db.Repo(repoName); ok {
			gitDir, source = r.GitDir, r.Source
		}
		db.Close()
	}
	fmt.Fprintf(&b, `# amux repo home — %s

You are the **home session** of repo **%s** (`+"`%s`"+`): the long-lived context for
the one-off agent sessions run against this repo. You know what has been tried on
it, by whom, and where it stands; you dispatch new one-off agents and steer the
ones running. Prompting this repo from the rail or the web reaches you.

## This repo
- Bare clone: `+"`%s`"+` — every agent worktree on this repo is sourced from it. Read
  history and files from it directly, e.g. `+"`git -C %s log --oneline origin/HEAD`"+`
  and `+"`git -C %s show origin/HEAD:<path>`"+`. Never write to it.
- You have no worktree of your own: to change code, dispatch a one-off agent and
  read its worktree at its sandbox path.

## Your sandbox
This directory (%s) is your writable sandbox; keep your notes here. Each one-off
agent's sandbox (listed below) holds its worktree of this repo, its own
`+"`CLAUDE.md`"+`, and its private config home with its transcript under `+"`.amux/`"+`.
Read freely; never edit an agent's worktree. %s

## Operate
%s
- `+"`amux do new-repo-agent %s -f prompt=\"…\"`"+` dispatches a new one-off agent on
  this repo (`+"`-f agent=claude|codex -f model=… -f mode=task|interactive`"+` are optional).

`, repoName, repoName, source, gitDir, gitDir, gitDir, home.Dir, guideRegenNote, steeringVerbs, repoName)
	b.WriteString(repoSessionsSection(repoName))
	b.WriteString("\n")
	b.WriteString(configHomeSection)
	b.WriteString("\n")
	b.WriteString(transcriptsSection)
	return b.String()
}

// membersSection lists a workgroup's agents from the store, so the coordinator
// starts with its roster. Best-effort: an unreadable store yields a note.
func membersSection(root store.Session) string {
	db, err := store.Open()
	if err != nil {
		return "## Your agents\n(could not read the store: " + err.Error() + ")\n"
	}
	defer db.Close()
	subs, _ := db.Children(root.ID)
	var b strings.Builder
	fmt.Fprintf(&b, "## Your agents (at launch, %s)\n", launchStamp())
	if len(subs) == 0 {
		b.WriteString("None yet — add one with the `add-agent` verb above.\n")
		return b.String()
	}
	for _, a := range subs {
		writeAgentLine(&b, a)
	}
	return b.String()
}

// repoSessionsSection lists the one-off agents on a repo: the members of every
// hidden repo-scoped workgroup for it, active first, then archived (capped).
func repoSessionsSection(repoName string) string {
	db, err := store.Open()
	if err != nil {
		return "## One-off sessions on this repo\n(could not read the store: " + err.Error() + ")\n"
	}
	defer db.Close()
	var active, archived []store.Session
	roots, _ := db.Roots()
	for _, r := range roots {
		if r.Scope != store.ScopeRepo || r.Role() == store.RoleRepo || firstOf(r.Repo) != repoName {
			continue
		}
		subs, _ := db.Children(r.ID)
		for _, a := range subs {
			if a.Archived || r.Archived {
				archived = append(archived, a)
			} else {
				active = append(active, a)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## One-off sessions on this repo (at launch, %s)\n", launchStamp())
	if len(active) == 0 {
		b.WriteString("No active one-off sessions — dispatch one with the `new-repo-agent` verb above.\n")
	}
	for _, a := range active {
		writeAgentLine(&b, a)
	}
	if len(archived) > 0 {
		sort.SliceStable(archived, func(i, j int) bool { return archived[i].ArchivedAt > archived[j].ArchivedAt })
		if len(archived) > guideArchivedCap {
			archived = archived[:guideArchivedCap]
		}
		b.WriteString("\nArchived (most recent):\n")
		for _, a := range archived {
			writeAgentLine(&b, a)
		}
	}
	return b.String()
}

// guideArchivedCap bounds how many archived agents a repo home's guide lists.
const guideArchivedCap = 10

// inventorySection is the console's launch-time snapshot of every workgroup,
// its agents, and every tracked repo with its one-off agents.
func inventorySection() string {
	db, err := store.Open()
	if err != nil {
		return "## Inventory\n(could not read the store: " + err.Error() + ")\n"
	}
	defer db.Close()
	var b strings.Builder
	fmt.Fprintf(&b, "## Inventory (at launch, %s — `amux status --json` for the live view)\n", launchStamp())
	roots, _ := db.Roots()
	repoAgents := map[string][]store.Session{}
	wrote := false
	for _, r := range roots {
		if r.Archived {
			continue
		}
		subs, _ := db.Children(r.ID)
		if r.Scope == store.ScopeRepo {
			if r.Role() == store.RoleRepo {
				continue
			}
			for _, a := range subs {
				if !a.Archived {
					repoAgents[firstOf(r.Repo)] = append(repoAgents[firstOf(r.Repo)], a)
				}
			}
			continue
		}
		if !wrote {
			b.WriteString("\n### Workgroups\n")
			wrote = true
		}
		fmt.Fprintf(&b, "- **%s** (`%s`) — coordinator sandbox `%s`\n", r.Display(), r.ID, store.RootDir(r.ID))
		for _, a := range subs {
			if a.Archived {
				continue
			}
			b.WriteString("  ")
			writeAgentLine(&b, a)
		}
	}
	if !wrote {
		b.WriteString("\n### Workgroups\nNone.\n")
	}
	repos, _ := db.Repos()
	b.WriteString("\n### Repos\n")
	if len(repos) == 0 {
		b.WriteString("None tracked.\n")
	}
	for _, r := range repos {
		fmt.Fprintf(&b, "- **%s** (`%s`) — home session id `%s`, bare clone `%s`\n", r.Name, r.Source, store.RepoHomeID(r.Name), r.GitDir)
		for _, a := range repoAgents[r.Name] {
			b.WriteString("  ")
			writeAgentLine(&b, a)
		}
	}
	return b.String()
}

// writeAgentLine renders one agent as a roster line: id, what it is doing, its
// runtime and mode, repos, branch, and sandbox path.
func writeAgentLine(b *strings.Builder, a store.Session) {
	label := strings.TrimSpace(a.Name)
	if label == "" {
		label = taskSummary(a.Prompt)
	}
	if label == "" {
		label = "(no name or prompt)"
	}
	fmt.Fprintf(b, "- `%s` — %s · %s/%s", a.ID, label, agent.Canonical(a.Agent), store.NormalizeMode(a.Mode))
	if a.Repo != "" {
		fmt.Fprintf(b, " · repos %s", a.Repo)
	}
	if a.Branch != "" {
		fmt.Fprintf(b, " · branch `%s`", a.Branch)
	}
	if a.Archived {
		b.WriteString(" · archived")
	}
	fmt.Fprintf(b, " · sandbox `%s`\n", a.Dir)
}

// taskSummary condenses an agent's initial prompt into one line (the first
// non-blank line, whitespace collapsed, capped), for roster lines.
func taskSummary(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if t := strings.Join(strings.Fields(line), " "); t != "" {
			if len(t) > 120 {
				t = t[:117] + "…"
			}
			return t
		}
	}
	return ""
}

func firstOf(list string) string {
	if r := store.SplitRepos(list); len(r) > 0 {
		return r[0]
	}
	return ""
}

func launchStamp() string { return time.Now().Format("2006-01-02 15:04 MST") }
