# Sandbox configuration: templates, private copies, and the feedback loop

Every amux agent runs its harness — Claude Code or Codex — inside a macOS Seatbelt or Linux/WSL2 bubblewrap
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
  Config can contain inline credentials, including MCP tokens. Template copying
  does not scrub these; review what you place in the host template and what you
  promote back. Hooks, plugins and shell/editor configuration are executable
  inputs, not just preferences. See [Security](../SECURITY.md).
- **Host auth by default.** Claude sessions inherit the host account. On macOS,
  a daemon credential broker forwards only the selected Claude account’s Keychain
  operations; sessions cannot access unrelated Keychain items. On Linux/WSL2,
  existing host credential-file sharing remains in place.
- **Optional separate login.** `amux auth login` establishes a dedicated Claude credential
  store shared by all its sessions, including credential-write and refresh locks.
  The scope mounts the directory so atomic credential replacement remains visible.
  Until enabled, Linux Claude retains the template credential symlink. Codex
  account auth uses a symlink; its MCP credentials use a hard link because Codex
  requires a regular file when writing refreshed tokens.

### Inherit the host Claude account

New Claude sessions use the host login automatically; no `amux auth login` is
needed. macOS preserves the host’s `CLAUDE_SECURESTORAGE_CONFIG_DIR` selector (or
`CLAUDE_CONFIG_DIR` when the former is unset). An explicit empty selector uses
the default host Keychain entry even though session settings and history live
in a private config directory. Host API-key and OAuth-token environment overrides
also remain available to the selected harness.

The macOS daemon publishes a protected `security` helper on the pane’s `PATH`.
Claude’s native credential reads, updates, and deletes go through the existing
signed session mailbox. The daemon derives the permitted account from host
configuration and the harness from its session records. Only the selected
`Claude Code` API-key and `Claude Code-credentials` entries are available.
Other accounts, other Keychain items, arbitrary commands, and keychain paths
are rejected. Archived or revoked sessions lose broker access. General Keychain
Mach services and the login keychain file remain blocked in the sandbox.

The broker operates on the original store; it does not copy a rotating refresh
token into a second account. Claude continues to perform OAuth refreshes and
coordinate with host Claude through the same `.oauth_refresh.lock`, legacy
credential-directory lock, and `.storage-write.lock`. Only those exact lock
paths are writable outside the session, not the host config/history tree.
A `/login` or `/logout` in a session changes this shared account, just as it would
in a host Claude terminal. The signed mailbox has bounded credential payloads
(currently 24 KiB per item); oversized values fail instead of being truncated.

`amux auth status` checks the active host or explicitly configured shared login.
`amux auth restart` reloads it in idle Claude panes; `--force` also interrupts
busy/unknown panes. Reopen existing terminal/editor tabs after upgrading to pick
up the helper and environment.

Codex PTY and App Server continue to inherit `auth.json` and MCP credential files.
The App Server uses the same sandbox and config grants as a Codex pane; it still
needs credentials, but file-backed Codex authentication does not need this
macOS Claude Keychain broker.

### GitHub CLI and Git HTTPS credentials

On macOS, every harness and its terminal/editor panes inherit the host's active
GitHub account through the daemon broker. A protected `gh` launcher requests the
token over the signed mailbox and passes it to that command and its children.
The token is not added to the harness environment or copied into an auth file.
The daemon invokes the host's native `gh auth token` for each request, preserving
GitHub CLI's Keychain encoding and active-account selection. Host account switches
take effect on the next command. Only configured hosts and the active account are
available; archived/revoked sessions, account overrides and credential mutations
are rejected. This grants sessions the same GitHub permissions as that account.

`gh api`, repository commands and `gh auth status` use the broker. The protected
Git configuration also routes HTTPS credential lookups through it, including
host configurations whose helpers name an absolute Homebrew `gh` path. Existing
Git identity/settings remain included. GitHub Enterprise host/repository overrides
are supported; token environment overrides explicitly set inside a session retain
GitHub CLI precedence. Run login, logout, refresh and account switching in a host
terminal. `gh auth status` in a session checks only the selected active account.

Use bare `gh` on the session's `PATH`: directly invoking the native binary by its
absolute path bypasses the launcher and cannot read macOS Keychain credentials.
Restart existing agent panes and reopen terminal/editor tabs after installing the
broker update so they receive its protected helpers, policy and Git configuration.
Linux/WSL2 retain their existing read-only GitHub configuration sharing.

### macOS certificate trust and restarting a session

The native sandbox blocks general Keychain IPC, which can prevent Codex from
enumerating TLS roots even when its login is available. amux supplies
`SSL_CERT_FILE=/etc/ssl/cert.pem` so Codex can verify HTTPS and WebSocket peers
using macOS's public PEM CA bundle. TLS verification remains enabled.

If the host daemon has `CODEX_CA_CERTIFICATE` or `SSL_CERT_FILE` set, amux keeps
that selection and grants read access to the specific certificate file. Set an
explicit PEM bundle for corporate/private roots; changes made only to the host
Keychain are not exported into the default bundle. Codex gives
`CODEX_CA_CERTIFICATE` precedence over `SSL_CERT_FILE`.

After updating the daemon, use **Alt+r** (then confirm) or
`amux do restart <id>` from the host to restart one Claude/Codex agent with its
saved conversation and current sandbox settings. This interrupts its current
turn. Workgroup coordinators restart independently of their agents, and
terminal/editor tabs remain running. For Codex App Server sessions, both the
supervised server and its attached agent UI are replaced using the saved thread.
The shortcut is configurable as `keys.restart-agent`.

Restarts automatically continue work that was running. Claude Code and Codex
PTY sessions receive a one-time continuation prompt when resuming an existing
conversation with an explicit running hook state. Idle, waiting-for-input and
unknown sessions reopen without submitting work. The creation prompt is not
replayed, and a missing transcript never receives an interrupted-work prompt.
Claude supplies this state through its hooks; Codex's App Server mode
uses the native state described below. Legacy Codex PTY mode has no built-in
running hook, so it reopens without automatic submission unless one is reported.
Enable native Codex goal tracking with `amux config set codex.control app-server`,
then restart the daemon to apply it.

For supervised Codex sessions, amux observes the native goal state before
shutdown. A previously active goal resumes automatically, preserving its
objective, token budget and usage. A goal paused before restart stays paused;
blocked, completed and budget/usage-limited goals are never reactivated. Codex
continues already-active goals itself, avoiding an extra model turn from amux.
An ordinary running turn without a native goal receives the continuation prompt.

The same behavior applies to `amux daemon restart`: its restore journal records
work state as well as running processes, including headless Codex supervisors.
Each saved session restores individually, so an archived workgroup member cannot
prevent its coordinator or siblings from restarting. Reopening or reconnecting
a dashboard does not submit continuation work.

An upgrade from an older daemon or PTY mode may have no saved native goal intent.
The new daemon cannot infer whether a stopped goal was running or deliberately
paused before that transition. It leaves the goal's stored status intact; the
native UI can still ask to resume it. A `blocked` goal is also left stopped.
Future restarts use the state observed by the upgraded daemon.

### Optional separate Claude login for all sessions

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
amux auth status          # status of the selected host/shared login
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

Each newly-created agent repository is a genuine Git linked worktree. Its small
common directory is private under the session and holds that session's writable
refs, config, hooks, objects, logs, index, and worktree administration. Large
base objects are read through a daemon-built immutable pool containing only the
authoritative remote default branch's reachable closure. The legacy shared bare
cache is never a pool source.

Pool generations are append-only. An unchanged upstream reuses its existing
generation; session creation accepts a current record for at most 30 seconds
before checking the remote again. A normal fast-forward fetches only its delta
into a new generation while Git sees the retained predecessor closure through a
process-scoped flat object list. Published pool generations contain no recursive
alternate files, so ordinary update counts cannot hit Git's alternate-depth
limit; existing packs are neither copied nor repacked. Source/default-branch
changes and non-fast-forward updates start a new root lineage, while a rollback
to a cached base selects only that base's prior authorized lineage. Old
generations remain for sessions already pinned to them. Session creation itself
transfers zero object payload and duplicates zero base-pack bytes: it initializes
only private metadata, lists the selected generation closure as flat alternates,
and checks out the assigned linked worktree.

The pane namespace consumes daemon-authoritative GitObjectMount values and binds
each exact generation objects directory read-only. It never binds a pool parent,
pool refs/config/hooks, the legacy cache, another session's common directory, or
the broad amux data/state roots. Objects readable through an explicit network
fetch with shared upstream credentials remain outside this filesystem boundary.

Worktree and private-common publication uses anchored renames from the
StateDir()/git-staging directory into session storage. State-directory staging
and session storage must be on the same filesystem. In particular, a custom
XDG_DATA_HOME that moves session storage onto another filesystem is unsupported.
EXDEV fails closed without copying or rewriting a live checkout.

The authoritative remote remains origin, so status/add/commit/stash/rebase,
fetch, push -u, pull, gh, editors, and hooks use normal Git behavior. Before the
assigned branch exists remotely, plain fetch follows only remote HEAD into
FETCH_HEAD; after push -u, pull follows the exact upstream without a wildcard
sibling refspec. Host lifecycle code never runs Git against the
session-writable common directory after publication. Deletion and validation
use a daemon-private typed layout record plus anchored filesystem operations, so
session config, fsmonitor, hooks, credential helpers, upload-pack settings,
includes, external commands, and alternate edits cannot influence host Git.

Network HTTPS/SSH sources are the confidentiality-supported pool inputs.
Filesystem and file: sources require the explicit
AMUX_GIT_TRUST_LOCAL_SOURCE=1 host acknowledgement and are safe only when no
session can write the source; repository-side upload-pack configuration is part
of that trust decision. They must not be described as confidential, and a
legacy amux bare cache is never eligible. Automatic submodule initialization is
not performed. Network submodules may be initialized normally into the
session-private common directory, subject to their own remote credentials;
local/shared submodule caches and relative local URLs have no isolation claim.

Legacy sessions created as linked worktrees are **not migrated automatically**.
After relaunch, amux refuses them explicitly instead of restoring writable access
to their shared bare Git common directory. Preserve the session's conversation
and inspect all dirty, staged, untracked, rebase and submodule state from the
host before recreating or using a future host-authorized migration tool. Already
running legacy mount namespaces keep their old writable-cache access until they
exit; installing a new binary cannot change an existing namespace.

An already-running session's sandbox policy is not changed by rebuilding the
binary. Relaunching a legacy shared-common worktree reaches the explicit refusal
above. Private full clones briefly created by the superseded #133 design remain
usable and isolated but are retained compatibility state, not the new creation
or migration path.

### What is configuration, what is state

The seed copies configuration and leaves per-machine state behind, so each copy
starts with an empty transcript tree, history, and caches:

| harness | copied (config) | not copied (state) | shared (auth) |
|---------|-----------------|--------------------|---------------|
| claude  | `settings.json`, `settings.local.json`, `CLAUDE.md`, `keybindings.json`, `statusline-command.sh`, `commands/`, `skills/`, `agents/`, `hooks/`, `output-styles/`, `plugins/`, `.claude.json` (minus its per-project trust table) | `projects/`, `history.jsonl`, `sessions/`, `session-env/`, `shell-snapshots/`, `file-history/`, caches, `statsig/`, `todos/` | host Keychain through the scoped daemon broker on macOS; host `.credentials.json` on Linux/WSL2; optional separate account after `amux auth login` |
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

### macOS Seatbelt

macOS uses `/usr/bin/sandbox-exec` with a default-deny profile inherited by all
children. It grants system tools read-only, the exact own session writable,
selected config/account paths, immutable Git objects read-only, and the
own Unix socket paths plus the system DNS broker. Original host paths are used; there are no bind mounts
or private PID namespace. Signals and process inspection are limited to the
same sandbox. IP networking remains shared. Native DNS resolution is permitted
through the exact system `/private/var/run/mDNSResponder` socket; other host Unix
sockets remain excluded. This is needed by model APIs and remote MCP servers,
even when outbound IP connections are allowed.

Browser authentication uses the native `/usr/bin/open` and LaunchServices APIs.
The profile allows application discovery/launch services, reads installed
`/Applications` bundles, and permits `lsopen`. **This is a broader host
application-launch capability, not a URL-only broker.** Seatbelt cannot filter
`lsopen` by URL scheme or restrict it to web browsers; an agent can launch other
applications outside its sandbox. Direct filesystem/socket denials below still
apply to session processes, but do not confine these launched host applications.
Browser profile directories, clipboard and Apple Events automation are not
added as direct grants.

`TestSeatbeltRuntimeDNSService` checks the native DNS broker in ordinary macOS
CI without depending on external Internet availability. Additional integration
checks are opt-in:

```sh
AMUX_TEST_NETWORK=1 go test ./internal/panespec -run '^TestSeatbeltRuntimePublicHTTPS$' -count=1 -v
AMUX_TEST_BROWSER=1 go test ./internal/panespec -run '^TestSeatbeltRuntimeBrowser' -count=1 -v
```

The HTTPS check uses unauthenticated model/MCP endpoints and verifies certificate
validation. The browser check opens two local test pages and requires actual
HTTP callbacks through both the command-line and native API launch paths.

`AMUX_SESSION_ACCESS` locates the daemon-owned read-only credential/context.
The signed subject and kernel policy determine authority; changing or clearing
the locator cannot read another credential or gain host control. The original
mailbox is read-only except for its own `requests` directory. The launcher
publishes a protected executable copy for bare `amux` and generated hooks, and
sets both `TMPDIR` and `CLAUDE_CODE_TMPDIR` to a private session temporary directory.
Inherited descriptors above standard error are closed before payload execution.

macOS does not support applying a second Seatbelt profile inside the first.
The protected launcher therefore disables Claude's inner sandbox and sets
Codex's inner mode to `danger-full-access`, including app-server thread policy.
**amux's outer Seatbelt profile remains enforced** and approval controls remain
unchanged. `AMUX_CODEX_SANDBOX=read-only` further makes the agent's session tree
read-only, except for private harness configuration, temporary files, and mailbox
requests. Shell/editor tabs retain their ordinary writable session grant.
New settings take effect only when the process is relaunched.


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

### Agent CLI mailbox access

All existing `amux agent ...` commands remain available through the ordinary
sandboxed shell. The launch's protected credential and per-session request mailbox
support Seatbelt on macOS and nested bubblewrap/harness policies on Linux. No
daemon socket exception or sandbox escalation is required. `name`/`label` and
`done` use the fixed launched subject; `sessions [--json]` uses a deliberate
read-only host discovery query. This query grants neither sibling mounts nor
cross-session write access. See [the dispatcher and transport audit](agent-cli-sandbox.md)
for flags, hook compatibility, restart/receipt semantics and validation limits.
