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
- **Workspace** — an agent + a chosen set of repos, identified by a short **id**.
  Each repo becomes a git worktree (branch `amux/<id>`) under
  `~/.local/share/amux/workspaces/<id>/`, and the agent opens there. A **name** is
  optional — the agent can set it later with `amux name "<summary>"`, and the rail
  shows it attached to the id. Workspaces are **persistent**: closing the window
  keeps the workspace; `x` (twice) deletes it and removes its worktrees/branches.
- **Mode** — a workspace is either a **task** (short-running, tied to a temporary
  task) or a **loop** (long-running, nearly autonomous). Shown in the rail with a
  distinct glyph (`●`/`○` for tasks, `∞` for loops).

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
- **Registry** — repos and workspaces persist in `~/.local/share/amux/registry.json`
  (atomic writes). Agent launch resolves the binary to an absolute path so a
  spawned window works even with a minimal server PATH.

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
| `Enter`     | open / switch to the selected workspace           |
| `n`         | new workspace (fzf repo picker in a popup)         |
| `x` `x`     | delete the workspace (twice to confirm)           |
| `R`         | force refresh                                      |
| `q`         | quit this dashboard pane                           |

Inside the isolated tmux server: prefix is `C-a`; `prefix + h` / `prefix + l`
(or `Alt+h` / `Alt+l`) move between the rail and your work; `prefix + g` opens
the full-screen dashboard; `prefix + a` opens the new-workspace creator.

## Commands

```
amux up                  # start daemon + isolated server, then attach
amux dash                # full-screen workspace dashboard
amux repo add <src>      # track a repo: git URL | local path | OWNER/REPO (via gh)
amux repo add            # no arg: fuzzy-find a GitHub repo (gh) and clone it
amux repo ls | rm <name> # list / untrack repositories
amux workspace new       # config page: repos, mode, prompt, name → create & open
amux workspace create <repo>... [--name n] [--prompt t] [--mode task|loop]   # scripting
amux workspace open <id> | rm <id> | rename <id> <name>
amux name <text>         # set the current workspace's name (run by the agent)
amux status              # print workspaces as text (scripting)
amux init                # (re)write the isolated tmux config
amux daemon              # run the daemon in the foreground (usually automatic)
```

## Layout

```
cmd/amux/            entrypoint, subcommands, repo/workspace CLI, embedded conf
internal/core/       Session model, wire protocol, paths
internal/store/      JSON registry of repos + workspaces (atomic writes)
internal/git/        bare clone + worktree helpers
internal/gh/         GitHub CLI wrapper (list/clone remote repos)
internal/agent/      agent-kind -> absolute argv resolver
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
