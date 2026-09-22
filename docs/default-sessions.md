# Default sessions: the console, workgroup coordinators, repo homes

Every container the rail shows hosts one long-lived agent session scoped to it.
These are amux's **default sessions**: they exist without being created, they
carry context about everything inside their container, and they are opened,
prompted, and read exactly like an agent — from the TUI, the CLI, and a remote
orchestrator over the provider protocol.

| container | role (`AMUX_ROLE`) | session id | sandbox (cwd) | scope (`AMUX_SCOPE`) |
|-----------|--------------------|------------|---------------|----------------------|
| the machine | `console` | `console` | `~/.local/share/amux/console/` | `global` |
| a work-scoped workgroup | `coordinator` | the workgroup id | `sessions/<workgroup>/coordinator/` — a dedicated own directory | `work` |
| a tracked repo | `repo` | the repo name | `sessions/<repo>/` | `repo` |

An ordinary agent has an empty role. A hidden single-member repo root (the
wrapper around a one-off agent) hosts no session.

## What each one is for

- **The console** has an explicit machine coordination view: every granted
  workgroup, agent, and repo, plus bounded normalized event queries for their
  history and the CLI to operate them. Other sessions' filesystem transcripts
  are not mounted. Its guide carries a launch-time
  inventory and the `amux do` vocabulary. It coordinates *across* workgroups;
  it does not write code.
- **A workgroup coordinator** supervises that workgroup's agents: scopes the
  work, dispatches (`amux do add-agent <workgroup> …`), steers (`amux do steer
  <agent> …`), verifies what agents produce against evidence, and keeps notes in
  the container dir (a `COORDINATION.md` is the convention). Prompting the
  workgroup — `Enter` on its rail row, `amux do steer <workgroup id> -f
  verb=prompt …`, or the web — reaches the coordinator. `amux do start
  <workgroup>` still starts the *members*.
- **A repo home** is the long-lived context for a repo's one-off agents: it
  knows what has been tried through its authenticated repo-scoped view and
  dispatches new one-offs (`amux do new-repo-agent <repo> …`). It does not mount
  another session's worktree or shared Git administration directly.

## Scope

A default session launches through the same typed bubblewrap boundary as an
agent: only its dedicated own directory is writable, with private PID/proc state,
fixed daemon-issued access mounts, and a private harness-config copy under
`<sandbox>/.amux/` (see `docs/sandbox-config.md`). A coordinator does not mount
the workgroup parent containing members; repo homes and the console do not mount
the amux data/state tree. Their wider views are explicit authenticated daemon
grants, so they change amux through the CLI and change code by steering an agent.

Existing processes keep the namespace they started with. A legacy coordinator
that still sees its member-containing parent or broad state invalidates any
mixed-mode confidentiality claim until every such runtime is stopped and safely
recreated. See `docs/namespace-rollout.md`; launch refuses unsupported legacy
roots rather than relocating or deleting unknown state during a read.

## Guides

Like an agent's, a default session's guide (`CLAUDE.md` / `AGENTS.md`) is
regenerated from the live store at every launch, so its roster and inventory
are current. Durable instructions belong in the session's private config home
memory (`.amux/claude/CLAUDE.md`), which survives regeneration.

## Lifecycle

- The console is synthetic (never a store row) and cannot be deleted.
- A workgroup's coordinator is the root row itself: creating the workgroup
  creates it (sandbox dir + pinned conversation) and starts its runtime, even
  in its dedicated `coordinator/` directory, even for an empty workgroup or a
  CLI/remote creation with no UI attached. Opening
  its rail row attaches to that running session. When the workgroup creation
  form includes a prompt, that is the coordinator's own task (see **Goal mode**
  below); `coordinator`/`coordinator_model` choose its runtime, while
  `agent`/`model`/`mode` describe a first member that repositories request. A
  member is created idle for the coordinator to dispatch, so the same task never
  runs twice, and a prompt alone creates no member at all. Without a prompt the
  coordinator starts ready for input. A root that predates default sessions gets both the first
  time it is resolved. Deleting the workgroup
  removes the coordinator's own files and leaves any agent sandbox that still
  lives under the container (a moved-out agent) untouched. Moving the last
  agent out of a work-scoped workgroup no longer drops the workgroup.
- A repo home is a repo-scoped root whose id is the repo name. Tracking a repo
  creates it; a repo tracked earlier gets one on first open. It goes with the
  repo (`amux repo rm`), and cannot be deleted as a workgroup.

## Wire

Published rows carry `role` (`harnessproto.Role*`) alongside `runtime` and
`caps`; see `docs/remote-provider-sessions.md` §2.2. A consumer that predates
the field sees a root or repo row with a runtime and caps, which is enough to
offer it the session affordances; one that sorts sectionless or `repo`-kind rows
into a session list should stop — the console and a repo header are not
one-off agents.

## Goal mode

A workgroup coordinator's job is to carry the user's task to a verified finish
with as little intervention as possible, so by default it runs on the runtime
that supervises exactly that natively: Codex's App Server thread goal
(`agent.GoalRuntime`). What this buys, and what it costs, in one place:

- **Every task becomes the goal.** The supervisor observes the canonical
  `userMessage` item the App Server broadcasts for *every* client — a rail or
  web `prompt`, the creation prompt, or a native TUI typing straight into the
  shared thread — and establishes it as the thread goal. There is no second
  kickoff turn: the goal is set inside the turn the message already started.
- **Codex continues it.** An active goal is continued by the runtime itself on
  every idle turn, with no TUI attached and nothing scheduled by amux. The
  objective, token budget and usage are the runtime's own, and `get_goal` /
  `update_goal` are the model's view of them. amux sets no budget of its own.
- **Only the user pauses it.** `amux do steer <workgroup> -f verb=goal
  -f status=active|paused|complete|clear [-f objective=…] [-f token_budget=N]`,
  and the same verb over the remote session wire, are the explicit controls. A
  paused or budget-limited goal stays that way: a further prompt runs as an
  ordinary turn rather than silently resuming what the user stopped. A `blocked`
  or usage-limited goal is resumed by the user's next message, which is the
  input it was waiting for. A completed goal is never reopened — the next task
  starts a fresh goal with its own accounting.
- **Restarts preserve it.** A restart (of the agent, or of the daemon) keeps the
  objective, progress and accounting, and resumes a goal that was active; this
  is the behaviour added in #158 and it is unchanged here.
- **Only the coordinator.** Ordinary member agents are unaffected: they keep
  their harness, their PTY or App Server control mode, and their upstream native
  goal tracking. amux neither establishes nor controls goals on their behalf,
  and `SessionCaps.Goal` advertises exactly that scope.

The default is a *runtime selection*, not a new engine: choosing another
coordinator harness at creation (`-f coordinator=claude`, or the Runtime field
on the interactive page) is supported and says so plainly — its rail status
reads `no goal mode (claude; tasks run as ordinary turns)` and its guide
describes the lifecycle it actually has. A goal session whose Codex has the
feature disabled fails to start with a clear error rather than pretending.
