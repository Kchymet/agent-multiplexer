# Sandbox configuration: templates, private copies, and the feedback loop

Every amux agent runs its harness — Claude Code or Codex — inside a bubblewrap
scope confined to the agent's own directory. This document describes how the
harness's *configuration* reaches that scope, why it is a copy rather than a
mount, and how an agent's edits to its configuration come back to amux.

## The model

- **Template.** Your own harness config dir — `~/.claude` (plus `~/.claude.json`)
  for Claude Code, `$CODEX_HOME` for Codex — is the template. amux never mounts it
  into a sandbox and never edits it on an agent's behalf.
- **Private copy.** When an agent is created (and again at every launch, if it is
  missing), amux seeds a copy of the template's *configuration* under the agent's
  dir: `<agent dir>/.amux/claude` or `<agent dir>/.amux/codex`. The harness is
  pointed at the copy with `CLAUDE_CONFIG_DIR` / `CODEX_HOME`, set for every pane
  of the agent — so `claude` typed in the agent's terminal tab uses it too. The
  copy is inside the one directory the scope already binds read-write, so it
  needs no mount of its own, and it is deleted with the agent.
- **The agent may edit its copy.** Settings, memory (`CLAUDE.md`), commands,
  skills, MCP servers, plugins — it is the agent's own configuration. What the
  agent does there stays there until you say otherwise.
- **Shared auth.** `amux auth login` establishes a dedicated Claude credential
  store shared by all its sessions, including credential-write and refresh locks.
  The scope mounts the directory so atomic credential replacement remains visible.
  Until enabled, Claude retains the legacy template credential symlink. Codex
  account auth uses a symlink; its MCP credentials use a hard link because Codex
  requires a regular file when writing refreshed tokens.

### One Claude login for all sessions

After installing the updated CLI **and restarting the amux daemon**, run from
your host terminal:

```sh
amux auth login
```

This runs Claude's own `auth login` in a dedicated context under
`<amux data>/auth/claude` (normally `~/.local/share/amux/auth/claude`). Complete
the browser login once. A successful login enables this store for new sessions
and queues the currently running Claude agent panes to resume their existing
conversations with it. Agents wait until their hooks report idle; a busy or
unknown-state pane is not interrupted automatically. Codex and editor/terminal
panes are not restarted. Reopen existing editor/terminal processes to inherit
the new environment. A failed/cancelled first login does not activate the store.

```sh
amux auth status          # Claude's status for the shared login
amux auth restart         # queue running Claude agents to resume when idle
amux auth restart --force # also interrupt busy/unknown agents stuck at login
```

`--force` applies to **all running Claude agent panes**, including ones doing
work. It resumes saved conversations but can interrupt an unfinished turn.
If the login succeeds but the daemon cannot accept the restart request, the CLI
reports that separately: the saved login is still usable. Update/start the
daemon and run `amux auth restart`. Pending reloads are local daemon work;
restarting that daemon restores its live agents with the current auth settings.
Restart failures are recorded in the daemon log; reopening that agent retries.

Claude uses `CLAUDE_CONFIG_DIR` for each agent's private config/history and
`CLAUDE_SECURESTORAGE_CONFIG_DIR` for the shared credential store. The shared
directory contains Claude's normal credentials and sibling locks; the directory
itself is never replaced. A refresh or `/login` in any migrated agent therefore
updates the same store. amux does **not** implement an OAuth refresh HTTP client
or start a second refresher: Claude's native cross-process locking coordinates
the writers. If a running process retains stale credentials, `amux auth restart`
reloads them. Normal refreshes do not restart sessions.

This is a new login, not a copy of the user's existing refresh token. Existing
private `.credentials.json` files are left on disk but are no longer selected or
mounted as the shared credential; they no longer appear as configuration drift.
The user's ordinary Claude login remains separate. Existing MCP definitions
are still copied from the user's config, but MCP credentials from that old
store are not imported: authenticate needed MCP servers in the shared context.
Inherited API-key, bearer-token and OAuth-token environment overrides are
cleared for migrated sessions so they cannot shadow the shared login. Explicit
credential helpers or alternate providers in Claude settings still need to be
configured consistently with the intended authentication method.

The separate secure-storage variable is currently an **undocumented Claude
interface** ([upstream tracking issue](https://github.com/anthropics/claude-code/issues/79223)).
Verified with Claude Code 2.1.263: its credential store, storage-write lock,
and OAuth refresh lock all use this directory. Use a current Claude Code
with this override and its native refresh locks.
The opt-in compatibility check uses synthetic credentials, not your login or a
model request:

```sh
AMUX_CLAUDE_AUTH_SMOKE=1 go test ./internal/claudecfg -run TestClaudeSharedAuthStoreCompatibility -v
```

This checks credential-store selection, not a real server-side token rotation.
The sandbox masks the auth root in every pane and mounts only a Claude pane's
selected store, so adding another harness's auth store does not expose it through
the otherwise-readable amux data tree.

### Git writes from Codex

An agent's worktree lives inside its workspace, but its Git index, objects and
refs live in the assigned bare clone under `<amux data>/repos`. amux grants that
clone write access in both the outer bubblewrap scope and Codex's inner
`workspace-write` sandbox. The same `sandbox_workspace_write.writable_roots`
override is passed to interactive Codex and App Server launches, including
resumed sessions. Other repositories and coordinator sessions receive no new
grants. Explicit read-only mode remains read-only.

Restart existing Codex sessions after updating amux to pick up these launch
arguments. An already-running session's sandbox policy is not changed by
rebuilding the binary.

### What is configuration, what is state

The seed copies configuration and leaves per-machine state behind, so each copy
starts with an empty transcript tree, history, and caches:

| harness | copied (config) | not copied (state) | shared (auth) |
|---------|-----------------|--------------------|---------------|
| claude  | `settings.json`, `settings.local.json`, `CLAUDE.md`, `keybindings.json`, `statusline-command.sh`, `commands/`, `skills/`, `agents/`, `hooks/`, `output-styles/`, `plugins/`, `.claude.json` (minus its per-project trust table) | `projects/`, `history.jsonl`, `sessions/`, `session-env/`, `shell-snapshots/`, `file-history/`, caches, `statsig/`, `todos/` | dedicated auth directory after `amux auth login`; legacy `.credentials.json` symlink until then |
| codex   | `config.toml`, `AGENTS.md`, `prompts/`, `skills/`, `rules/` | `sessions/`, `history.jsonl`, `log/`, `memories/` | `auth.json`, `.credentials.json` (MCP OAuth), `mcp-oauth-locks/` |

### MCP inheritance

Codex agents inherit `[mcp_servers.*]` from your `config.toml`, including server
URLs, commands, environment settings, and tool policies. Claude agents inherit
user-level `mcpServers` from `.claude.json`. Each harness inherits its own
configuration; amux does not translate MCP definitions between harnesses.

For Codex MCP servers authenticated with OAuth, such as Linear, the file-backed
login lives in `$CODEX_HOME/.credentials.json`, separately from the account
login in `auth.json`. amux shares this file and the `mcp-oauth-locks/` directory,
so token refreshes use the same credentials and locks as the user's Codex.
The credential hard link requires the template and agent home to be on the same
filesystem; amux reports a seeding error if it cannot create the link. OS keyring
credentials continue to depend on Codex's configured backend and its availability
inside the sandbox; amux does not export keyring secrets into files.

For Codex and legacy Claude auth, missing shared credential links are added at the next launch, including for
existing agents and logins completed after an agent was created. Existing config
edits and private credential files are preserved. To update an existing agent's
MCP definitions, use `amux sandbox reset <id> config.toml` (this resets the whole
config file). For a detached MCP credential, use
`amux sandbox reset <id> .credentials.json`. Relaunch the agent after either reset.
Existing private lock directories are overlaid with the shared directory inside
the sandbox. When running with the amux sandbox disabled, an existing private
lock directory must be reconciled before concurrent OAuth refreshes can share
locks; newly seeded homes link to the shared lock directory directly.

Two files get a small transform on the way in. `settings.json` has absolute
references to the template dir rewritten to the copy, so a status-line script or
hook command under `~/.claude` runs the copy's file inside the scope (where
`~/.claude` does not exist). `.claude.json` is copied without its `projects` table
(your trust and history for your own directories); amux trusts the agent's own dir
in the copy at launch.

Transcripts therefore live in the agent's private home. Resume detection,
gap-fill from amux's captured backups, `amux agent sessions`, and the runtime
event stream all read each agent's home (and the user's, for your own sessions).
An agent created before this change has its conversation in your `~/.claude`;
its first launch afterwards carries that project dir over, once, so nothing is
lost — and until it launches, readers fall back to the old location.

## The feedback loop

Because a copy could otherwise drift from the template in silence, amux records
what each copy was seeded with — a manifest of content hashes, kept under amux's
state dir (`~/.local/state/amux/cfghome/`), which is *not* bound into any
sandbox, so an agent cannot rewrite its own baseline. Comparing copy, template,
and manifest attributes every difference:

| status | meaning | typical decision |
|--------|---------|------------------|
| `agent-changed` / `agent-added` / `agent-removed` | the agent edited its copy; the template still has what was seeded | `promote` (propagate) or `reset` (discard) |
| `template-changed` | you changed the template since the seed; the copy is stale | `reset` pulls the new version into the copy |
| `conflict` | both sides changed the same path differently | pick one with `promote` / `reset` |
| `shared-detached` | a legacy linked auth file was replaced with a private file | `reset` re-links it to yours; use `amux auth login` for durable Claude sharing |

Files the harness churns on its own are compared on their configuration only:
`.claude.json` on its config keys (`mcpServers`, `model`), `config.toml` without
the `[projects.*]` trust tables amux writes at launch, `settings.json` with the
path rewrite mapped back. So a launch, a startup counter, or a granted trust never
reads as an edit.

Where you see it:

- **The rail.** The daemon's poll scans each live agent's copies (throttled to
  every 10s) and appends `⚙ N config edits` to the agent's status line while any
  edit awaits a decision. The daemon log records the moment an agent's pending
  edits first appear or change.
- **`amux sandbox drift [<id>] [--json]`** lists the diverged paths per agent with
  the paths of both versions, so you can diff them.
- **`amux sandbox promote <id> <path>`** copies the agent's version into the
  template — into your config, and so into every agent seeded from now on.
  `.claude.json` is merged on its config keys, never overwritten, so your own
  state in it survives.
- **`amux sandbox reset <id> <path>`** re-copies the template's version into the
  agent (or removes a file the template lacks; or re-links a detached credential).
- **`amux doctor`** summarizes how many agents have edits awaiting a decision.
- **`amux sandbox path <id>`** prints an agent's private config dir(s) and the env
  that points the harness at them.

Nothing propagates in either direction on its own. Where both sides end up
agreeing anyway — you made the same change by hand, or a promoted change settled
— the path drops out of the listing.

## Why a copy, not a mount

Mounting `~/.claude` read-write into every sandbox (the previous design) meant
every agent shared one configuration with you and with each other: an agent that
added an MCP server or changed a permission changed it for everyone, immediately
and invisibly, and the whole tree — your transcripts and history included — was
readable and writable from inside the scope. A private copy gives each agent its
own configuration to own, limits the shared harness paths to credentials and
OAuth locks, and turns configuration change into an explicit, reviewable event.

The trade-off is intentional: nothing an agent configures reaches you unless you
promote it, and configuration changes reach an existing agent when you reset
its copy (new agents are seeded from the current template); missing shared
credential links are filled in at launch. The agent guide
(`CLAUDE.md` / `AGENTS.md`) tells the agent all of this.

## Code map

| package | role |
|---------|------|
| `internal/cfghome` | the generic machinery: `Seed`, `Scan`, `Promote`, `Reset`, `Binds`, the manifest, the rail summary cache |
| `internal/claudecfg` (`template.go`, `Home`) | what a Claude home is; which entries are config vs state vs auth; the `.claude.json` / `settings.json` transforms; every transcript lookup on an explicit `Home` |
| `internal/codexcfg` (`template.go`, `Home`) | the same for Codex |
| `agent.Harness.Config` | a harness's spec for an agent (kind, env var, template, copy); `PrepareLaunch`, `Activity`, `RestoreTranscript`, `RuntimeTranscriptPath`, `ListSessions` all read the agent's home |
| `wsops.ensureConfigHome` | seeds at creation and at every launch; `AgentEnv` exports the config env |
| `panespec.configBinds` | binds shared auth and OAuth locks for the agent pane |
| `source.configSuffix` | the rail's `⚙ N config edits` |
| `cmd/amux/sandbox.go` | `amux sandbox drift / promote / reset / path` |
