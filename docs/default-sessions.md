# Default sessions: the console, workgroup coordinators, repo homes

Every container the rail shows hosts one long-lived agent session scoped to it.
These are amux's **default sessions**: they exist without being created, they
carry context about everything inside their container, and they are opened,
prompted, and read exactly like an agent — from the TUI, the CLI, and a remote
orchestrator over the provider protocol.

| container | role (`AMUX_ROLE`) | session id | sandbox (cwd) | scope (`AMUX_SCOPE`) |
|-----------|--------------------|------------|---------------|----------------------|
| the machine | `console` | `console` | `~/.local/share/amux/console/` | `global` |
| a work-scoped workgroup | `coordinator` | the workgroup id | `sessions/<workgroup>/` — the container dir holding every member's sandbox | `work` |
| a tracked repo | `repo` | the repo name | `sessions/<repo>/` | `repo` |

An ordinary agent has an empty role. A hidden single-member repo root (the
wrapper around a one-off agent) hosts no session.

## What each one is for

- **The console** has machine-wide context: every workgroup, agent, and repo,
  what each agent was asked to do and what it did (their transcripts are
  readable), and the CLI to operate all of it. Its guide carries a launch-time
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
  knows what has been tried, dispatches new one-offs (`amux do new-repo-agent
  <repo> …`), and reads the bare clone directly. It has no worktree of its own.

## Scope

A default session launches through the same path as an agent: a bubblewrap
scope with its sandbox writable, the amux data tree readable, and a private
copy of your harness config under `<sandbox>/.amux/` (see
`docs/sandbox-config.md`). That is what makes the scoping fall out of the
directory layout: the coordinator's sandbox *is* the container that holds its
members' sandboxes, so it can read every member's worktree, guide, and
transcript; the repo home and the console read the data tree. None of them gets
a writable bare clone — they change amux through the CLI and change code by
steering an agent, and their guides say so.

## Guides

Like an agent's, a default session's guide (`CLAUDE.md` / `AGENTS.md`) is
regenerated from the live store at every launch, so its roster and inventory
are current. Durable instructions belong in the session's private config home
memory (`.amux/claude/CLAUDE.md`), which survives regeneration.

## Lifecycle

- The console is synthetic (never a store row) and cannot be deleted.
- A workgroup's coordinator is the root row itself: creating the workgroup
  creates it (sandbox dir + pinned conversation); a root that predates default
  sessions gets both the first time it is resolved. Deleting the workgroup
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
