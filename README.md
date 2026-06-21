# amux

An **AI-native terminal control plane**. Open your terminal and land directly in
a dedicated, isolated tmux server whose persistent side-pane is a **workspace
switcher**. A workspace bundles an agent (Claude) with a set of tracked
repositories, each materialized as a git **worktree**, all opened together in one
window. Create, switch, and delete workspaces without leaving the view.

```
┌──────────────────────────────┬───────────────────────────────────────────────┐
│ amux · workspaces            │ claude  (workspace: payments-fix)               │
│ ● payments-fix               │ > …                                             │
│   running · 2 repos          │   (worktrees: api/  web/)                        │
│ ○ refactor-auth              │                                                 │
│   idle · 1 repo              │                                                 │
│ ○ spike-graphql              │                                                 │
│   idle · 3 repos             │                                                 │
│ ──────────────────────────── │                                                 │
│ ↵ open    n new              │                                                 │
│ x×2 del   R ↻                │                                                 │
└──────────────────────────────┴───────────────────────────────────────────────┘
   rail = workspace switcher       the workspace's agent
```

## Why

You juggle agents across many repos with no single place to see or switch between
them. amux gives you one: workspaces (agent + repos-as-worktrees) listed in a
persistent rail, running in their **own tmux server** (`tmux -L amux`), fully
separate from your default tmux and `~/.tmux.conf`.

## Concepts

- **Repository** — a tracked repo. `amux repo add` clones it as a local **bare**
  clone (the worktree source) under `~/.local/share/amux/repos/`. Sources can be
  a git URL, a local path, an `OWNER/REPO` shorthand, or — via the **GitHub CLI**
  (`gh`) — fuzzy-found from your remotes: first pick an **owner** (your account or
  an org you belong to), then a repo. If `gh` isn't authenticated, amux prompts
  you to `gh auth login` rather than falling back. Reachable from `amux repo add`
  (no argument) or the "clone a new repo…" entry in the workspace picker.
- **Session** — a **root** container (a short **id**, optional name) holding one
  or more **sub-sessions**. Each sub-session binds **one agent to one worktree**:
  a repo on a branch (`amux/<id>`) under `~/.local/share/amux/sessions/<root>/`,
  or a plain agent with no repo. Sub-sessions under one root can be the **same
  repo on different branches**, **different repos**, or a mix — and each can use a
  **different agent/model**. The rail nests sub-sessions under their root. A name
  is optional (`amux name "<summary>"`); sessions are **persistent** and resume
  across restarts. `x` (twice) deletes — a root removes all its sub-sessions.
- **Mode** — a workspace is either a **task** (short-running, tied to a temporary
  task) or a **loop** (long-running, nearly autonomous). Shown in the rail with a
  distinct glyph (`●`/`○` for tasks, `∞` for loops).
- **Control console** (`⚙`, pinned first in the rail) — a built-in session that
  runs an agent in a neutral directory, preconfigured (via its `CLAUDE.md`) to help
  you configure and operate amux. It is scoped to amux **configuration + CLI only**
  — it won't touch workspace code or the amux source. Open it from the rail, with
  `C-a C`, or `amux console`. Edit `~/.local/share/amux/console/CLAUDE.md` to tailor
  how it behaves.

## Philosophy: amux is a UI layer

amux switches, displays, and launches — it does **not** orchestrate. Any
autonomy (auto-accept edits, looping, retries) belongs to the **agent itself**
(local) or to a **cloud orchestrator**, never to amux. To make that work amux
exports the session's intent on the agent's window and gets out of the way:

| env var          | meaning                                   |
|------------------|-------------------------------------------|
| `AMUX_MODE`      | `task` (short) or `loop` (long/autonomous)|
| `AMUX_WORKSPACE` | the workspace id                          |
| `AMUX_AGENT`     | the agent kind (`claude`)                 |

The launch binary is overridable with `AMUX_CLAUDE_BIN`, so you point amux at
your own wrapper that decides how to run based on `$AMUX_MODE` — e.g. add
`--permission-mode acceptEdits` and start a `/loop` for `loop` sessions. A
reference wrapper is `scripts/claude-launch.example.sh`:

```sh
cp scripts/claude-launch.example.sh ~/.config/amux/claude-launch.sh
export AMUX_CLAUDE_BIN="$HOME/.config/amux/claude-launch.sh"   # in your shell rc
```

amux ships **no** autonomy policy of its own; `mode` is intent + display only.

### Launch defaults: trusted + auto

For agents amux spawns, it sets two sensible (overridable) defaults so a fresh
worktree is usable without prompts:

- **Trusted by default** — amux pre-marks each directory it creates as trusted in
  `~/.claude.json` (`projects.<dir>.hasTrustDialogAccepted`), so Claude Code skips
  its interactive "trust this folder?" dialog. (amux only trusts dirs it created.)
- **Auto permission mode** — agents launch with `--permission-mode auto`: a
  classifier auto-approves safe operations and blocks escalations. This is **not**
  `--dangerously-skip-permissions`/`bypassPermissions`. Override per kind with
  `AMUX_PERMISSION_MODE=default|acceptEdits|plan|auto` (or `none` to omit), or take
  full control with an `AMUX_CLAUDE_BIN` wrapper.

## How it works

- **Isolated tmux server** — everything runs under the `amux` socket with its own
  config (`~/.config/amux/amux.conf`). Your existing tmux setup is never touched.
- **Daemon** — one background process reads the registry (~2s), tracks which
  workspaces are running (via a `@amx_ws` window tag), and serves state + control
  actions (open / delete) over a unix socket.
- **Rail** — a narrow workspace-switcher pane auto-added to the left of *every*
  window via a tmux hook. A thin client of the daemon, so N rails ≠ N pollers.
- **New workspace flow** — `n` opens a tmux popup with a **configuration page**
  (an fzf menu): drill into **Repos** (multi-select, with an inline gh/clone
  "add a repo…" entry), **Mode** (task/loop), an optional **Prompt**, and an
  optional **Name**, then choose *Create*. Only repos are required. The initial
  prompt seeds the agent on launch (Claude's positional prompt).
- **Store** — repos and sessions persist in a **SQLite** DB at
  `~/.local/share/amux/amux.db` (sessions carry a `root_id`; WAL mode lets the
  daemon read while the CLI writes). A pre-existing JSON `registry.json` is
  imported once and kept aside as `registry.json.migrated`.

## Install

Requires Go 1.24+ and tmux 3.x.

```sh
make install
```

This builds the binary to `~/.local/bin/amux`, writes the isolated tmux
config, and installs the shell shim. Then add the printed line to your shell rc:

```sh
[ -f "$HOME/.config/amux/amux.sh" ] && . "$HOME/.config/amux/amux.sh"
```

Make sure `~/.local/bin` is on your `PATH`. Open a new terminal and you're in.

> Escape hatch: `AMUX_SKIP=1` in the environment disables auto-launch, so a
> plain shell / your default tmux is always reachable (`AMUX_SKIP=1 zsh`).

### Windows Terminal (WSL2)

Add a dedicated profile so opening the terminal drops you straight into amux,
leaving your other profiles intact. In Windows Terminal *Settings → Add a new
profile*, set the command line to your login shell (which sources the shim), e.g.

```
wsl.exe -d <your-distro> -- zsh -lic 'exec amux up'
```

Then set this profile as the default. (Equivalently, just keep your normal WSL
profile — sourcing the shim from `~/.zshrc` already auto-launches amux.)

### macOS

The shell shim covers iTerm2 / Terminal automatically — no extra step. Optionally
create a dedicated iTerm profile whose command is `amux up`.

## Keys (rail & dashboard)

| key         | action                                            |
|-------------|---------------------------------------------------|
| `↑/↓`,`k/j` | move selection                                    |
| `Enter`     | open a sub-session / open all of a root's agents  |
| `n`         | new session (config page in a popup)              |
| `a`         | add an agent (sub-session) to the selected root   |
| `x` `x`     | delete (root removes all its sub-sessions)        |
| `R`         | force refresh                                      |
| `q`         | quit this dashboard pane                           |

Inside the isolated tmux server: prefix is `C-a`; `prefix + h` / `prefix + l`
(or `Alt+h` / `Alt+l`) move between the rail and your work; `prefix + g` opens
the full-screen dashboard; `prefix + a` opens the new-workspace creator;
`prefix + C` opens the control console; `prefix + d` detaches.

## Commands

```
amux up                  # start daemon + isolated server, then attach
amux dash                # full-screen workspace dashboard
amux repo add <src>      # track a repo: git URL | local path | OWNER/REPO (via gh)
amux repo add            # no arg: fuzzy-find a GitHub repo (gh) and clone it
amux repo ls | rm <name> # list / untrack repositories
amux session new         # config page: name + add agents (repo/branch/mode/model) → open
amux session add <root>  # add an agent (sub-session) to a root
amux session create <repo>... [--name n] [--prompt t] [--mode task|loop] [--model m]
amux session open <id> | rm <id> | rename <id> <name> | ls
amux name <text>         # set the current session's name (run by the agent)
amux console             # open the amux control console (configure amux)
amux status              # print workspaces as text (scripting)
amux init                # (re)write the isolated tmux config
amux daemon              # run the daemon in the foreground (usually automatic)
```

## Layout

```
cmd/amux/            entrypoint, subcommands, repo/workspace CLI, embedded conf
internal/core/       Session model, wire protocol, paths
internal/store/      SQLite store: repos + sessions (root_id hierarchy) + migration
internal/git/        bare clone + worktree helpers
internal/gh/         GitHub CLI wrapper (list/clone remote repos)
internal/agent/      agent-kind -> absolute argv resolver (+ permission mode)
internal/claudecfg/  safe edits to ~/.claude.json (pre-trust spawned dirs)
internal/console/    the control console: neutral dir + preconfigured CLAUDE.md
internal/wsops/      workspace open/create/delete (shared by daemon + CLI)
internal/source/     Source interface + workspace.go (claude/hermes/tmux stubs)
internal/daemon/     registry poll, unix-socket server, actions, client
internal/tmuxctl/    thin `tmux -L amux` wrapper (+ @amx_ws tagging)
internal/tui/        Bubble Tea model: rail (switcher) + dash (full)
scripts/amux.sh      shell shim
```

## Limitations / roadmap

- One agent kind for now (**Claude**); the `agent` resolver and `Agent` field are
  ready for Hermes (`internal/agent`).
- Worktrees branch from the bare clone's HEAD; pulling upstream updates into the
  store isn't wired yet.
- No per-repo branch choice yet (all repos in a workspace share `amux/<name>`).
