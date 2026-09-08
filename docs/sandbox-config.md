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
The sandbox never mounts the amux data or auth root. It mounts only a Claude
pane's exact selected store, so adding another harness's auth store does not
expose it through a shared ancestor.

### Git writes from Codex

Each newly-created agent repository is a fully independent clone inside that
session's workspace. Its `.git` directory contains the session's own config,
refs, hooks namespace, index, worktree metadata and objects. amux clones only the
authoritative source's default branch, without local hardlinks, tags or object
alternates; it does not seed from the mutable host cache. Consequently a
sibling's unpublished branch/object is not copied into a new session, and Git
writes need no mount outside the session directory in either the bubblewrap or
Codex `workspace-write` sandbox.

Checkout publication is an atomic rename from daemon-private
`StateDir()/git-staging` into session storage. Those locations must be on the
same filesystem. In particular, a custom `XDG_DATA_HOME` must not place session
storage on a different filesystem from amux's state directory. If the kernel
returns `EXDEV`, creation fails closed: amux does not publish a partial checkout
or fall back to a non-atomic copy.

This clone policy is a write-isolation and initial-transfer boundary. The pane
namespace supplies the corresponding read boundary: it mounts only the exact
session directory and never the amux data/state roots, sibling clones, or host
Git cache. An explicit network fetch can still retrieve objects the remote
source advertises.

The remote remains the tracked repository's authoritative source, so ordinary
fetch/commit/push and pull-request workflows keep working. Host-side lifecycle
code does not invoke Git against the private clone after launch: the session can
edit its local Git config, so hooks, fsmonitor, credential helpers, includes and
other command-bearing settings are untrusted. Deletion removes a validated
session path through directory-FD-anchored filesystem operations rather than
asking that repository to run Git. A daemon-private layout record, not mutable
`.git` contents, selects independent versus legacy cleanup.

Legacy sessions created as linked worktrees are **not migrated automatically**.
After relaunch, amux refuses them explicitly instead of restoring writable access
to their shared bare Git common directory. Preserve the session's conversation
and inspect all dirty, staged, untracked, rebase and submodule state from the
host before recreating or using a future host-authorized migration tool. Already
running legacy mount namespaces keep their old writable-cache access until they
exit; installing a new binary cannot change an existing namespace.

An already-running session's sandbox policy is not changed by rebuilding the
binary. Relaunching a legacy linked-worktree session reaches the explicit refusal
above; only newly-created independent clones launch normally in this release.

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
the sandbox. Protected launches refuse a disabled or unsupported namespace
rather than forwarding session credentials to a host-visible process; newly
seeded homes link to the shared lock directory directly.

Two files get a small transform on the way in. `settings.json` has absolute
references to the template dir rewritten to the copy, so a status-line script or
hook command under `~/.claude` runs the copy's file inside the scope (where
`~/.claude` does not exist). `.claude.json` is copied without its `projects` table
(your trust and history for your own directories); amux trusts the agent's own dir
in the copy at launch.

Transcripts therefore live in the agent's private home. Host-side resume and
runtime readers operate on that explicitly selected home; restricted sessions
receive only role-filtered context through authenticated amux requests, never a
global transcript path. An agent created before this change has its conversation
in your `~/.claude`; its first launch afterwards carries that project dir over,
once, so nothing is lost — and until it launches, host-authorized readers fall
back to the old location.

### Namespace grants

On supported Linux hosts, protected panes require bubblewrap 0.12.0 or newer
and enter a private PID namespace with a fresh `/proc`. They receive the exact
session directory, selected runtime/config/account grants, their own App Server
socket directory, and daemon-issued file-RPC mounts. `/run`, amux data/state,
global hooks/transcripts, sibling directories and shared Git metadata are not
mounted. The mailbox is read-only except for its `requests/` overlay;
credentials and fixed `context.json` are read-only at the immediate-root
`/amux-session-access` directory. Host provider/TLS/management environment
variables and ambient API tokens are removed before the child starts. Immediately
before the final payload exec, an in-namespace trampoline marks every inherited
descriptor above standard error close-on-exec; a private `/proc` alone cannot
revoke an already-open host file descriptor.

The 0.12.0 floor is a security boundary, not a packaging preference. The
[bubblewrap advisory](https://github.com/containers/bubblewrap/security/advisories/GHSA-pxhw-h44j-8pfx)
marks older releases vulnerable to following an attacker-controlled mount-target
symlink through the setup-time `/oldroot`; 0.12.0 creates destinations with
`openat2(RESOLVE_IN_ROOT)`. amux refuses an older or missing binary rather than
falling back to a broad host view. Runtime acceptance on a host with an older
binary should use a disposable Linux VM or CI runner image that already contains
bubblewrap 0.12.0 or newer and enables unprivileged user and PID namespaces. This
tests the real mount/PID boundary without installing packages, restarting the
host daemon, or nesting a harness sandbox probe on the development host.

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
