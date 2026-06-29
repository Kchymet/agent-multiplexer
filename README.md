# amux

An **AI-native terminal control plane** for orchestrating coding agents across
many repositories. `amux` opens into a native full-screen UI: a switcher rail on
the left, the selected agent embedded on the right. Agents are grouped into
**workgroups**, each agent gets its own git **worktree** and a row of **tabs**
(the agent, an editor, a jailed shell), and the whole thing is split into a
**client/server** architecture so one UI can drive a local multiplexer *and* any
number of remote ones.

```
┌ amux ──────────────── ⌥ l ▸┬ 1 agent  2 editor  3 term ──────────────────────┐
│ ⚙ amux console            │ ✻ Working… (payments-fix)                          │
│                           │ > implement the idempotency key on POST /charges   │
│ WORKGROUPS                │                                                    │
│ ▸ payments-fix            │   (worktrees: api/  web/)                          │
│   ● idempotency           │                                                    │
│   ◐ web-banner            │                                                    │
│ REPOS                     │                                                    │
│ ⛁ acme/api                │                                                    │
│   ● hotfix-auth           │                                                    │
│ ⛁ acme/web                │                                                    │
│ ──────────────────────────┴────────────────────────────────────────────────  │
│ ↵ open  ↑↓ move           │ a +agent · w +group · m move · x done · q quit     │
└───────────────────────────┴────────────────────────────────────────────────────┘
   rail = workgroup / repo switcher       the focused agent's current tab
```

## Why

You run many agents across many repos with no single place to see, switch, and
control them. amux is that place: a switcher rail plus an embedded view of the
focused agent, with first-class worktrees, per-agent editor/terminal tabs, and a
backend you can run locally or on a remote box.

---

## System design

amux is split into three roles connected by two wire protocols. The UI owns no
agent processes; the **multiplexer server** owns the model and routes I/O; the
**agent harness** owns the actual PTYs. (See `docs/client-server.md`.)

```
        UI ⇄ Server protocol                     Server ⇄ Harness protocol
          (muxproto, line-JSON)                    (harnessproto, line-JSON)
┌──────────────┐   unix / TCP    ┌────────────────────────┐   stdio / pipe   ┌───────────────┐
│   UI client  │ ───────────────▶│   Multiplexer Server   │ ────────────────▶│ Agent Harness │
│ (native TUI) │◀─ snapshots,    │      (amux serve)      │◀─ pane output,   │  (amux harness)│
│  renders     │   pane output   │  • store  (SQLite)     │   exit           │  • PTY per pane│
│  vterms      │ ─ actions,      │  • source (rail state) │ ─ spawn, input,  │  • claude /    │
│  forwards    │   pane input ──▶│  • wsops  (lifecycle)  │   resize, kill ─▶│    editor /    │
│  keystrokes  │                 │  • panespec (pane spec)│                  │    jailed shell│
└──────────────┘                 └────────────────────────┘                  └───────────────┘
   one UI can connect to a local server and many remote servers at once
```

- **Local & remote** — `amux serve` listens on a local unix socket and, if asked
  (`amux serve tcp:0.0.0.0:7077`), on TCP. A UI points at a server with
  `AMUX_SERVER` (`host:port`, or empty for local). A remote server orchestrates
  agents on *its* machine; the UI just renders bytes.
- **Why a harness** — putting pane execution behind a protocol is what lets the
  server run agents locally, in a jail, in a container, or over ssh on another
  host, without the server (or UI) caring.

### Components

| Component | Package | Responsibility |
|-----------|---------|----------------|
| **UI / native TUI** | `internal/nativetui` | The full-screen client: rail switcher, embedded agent panes (vterm), per-agent tabs, modal forms/confirms, all keybindings. Renders; owns no agent lifecycle. |
| **Multiplexer server** | `internal/mux` | The backend (`amux serve`). Serves `muxproto` to UI clients over unix/TCP, broadcasts rail snapshots, applies lifecycle actions, and multiplexes pane I/O between clients and a harness. |
| **Agent harness** | `internal/harness` | Owns the real processes (`amux harness`). Spawns each pane in a PTY, streams its output, accepts input/resize/kill. The unit that can run jailed/remote. |
| **UI ⇄ Server protocol** | `internal/muxproto` | Message types + helpers for hello/welcome, subscribe/snapshot, action/result, and pane open/input/resize/close/output/exit. |
| **Server ⇄ Harness protocol** | `internal/harnessproto` | Message types for spawn/input/resize/kill and ready/output/exit. |
| **Wire transport** | `internal/wire` | Shared line-framed JSON codec used by both protocols over any stream (unix/TCP/stdio/pipe). |
| **UI client lib** | `internal/muxclient` | Dials a local/remote server and exposes its state stream + per-pane I/O to a UI. |
| **Session model / store** | `internal/store` | SQLite store (`~/.local/share/amux/amux.db`): repos, sessions (a one-level `root_id` tree), `scope` (work/repo) and `archived` flags, idempotent migrations. |
| **Rail state** | `internal/source` | Derives the rail snapshot (`[]core.Session`) from the store: console, workgroups + nested agents, repos + their agents, archived, detached. |
| **Lifecycle ops** | `internal/wsops` | Create/add/move/archive/delete workgroups & agents, worktree setup, and the shared action dispatch (`Apply`). Used by the server, daemon, and CLI. |
| **Pane spec** | `internal/panespec` | Resolves what to run for a tab (agent / editor / shell) and scopes every pane to the agent's worktree (bubblewrap), shared by the TUI and the server. |
| **Shared types** | `internal/core` | The normalized `Session`, the `Action` wire type, well-known paths/sockets. |
| **Agent resolver** | `internal/agent` | Maps an agent kind to an absolute argv (+ permission mode); robustly finds `claude` even when version managers keep it off PATH. |
| **Embedded terminal** | `internal/vterm` | A VT emulator over a PTY (and, for the client path, over a byte stream) so a full-screen agent renders inside a pane. |
| **Control console** | `internal/console` | The built-in `⚙` agent scoped to amux config/CLI. |
| **Git / GitHub** | `internal/git`, `internal/gh` | Bare clones + worktrees; `gh`-driven repo discovery/clone. |
| **Claude config** | `internal/claudecfg` | Safe edits to `~/.claude.json` (pre-trust spawned dirs), status hooks, model defaults. |
| **Legacy daemon / tmux** | `internal/daemon`, `internal/tmuxctl`, `internal/tui` | The original tmux-rail front-end and its poller (`amux up`/`dash`), kept working alongside the native TUI. |

> Status: the protocols, server, harness, and client are complete and covered by
> an end-to-end test (`internal/mux`). The default `amux` native TUI still spawns
> panes locally; migrating it onto the client (so the default app is a thin client
> of `amux serve`) is the final wiring step.

---

## Concepts

- **Repository** — a tracked repo, cloned as a local **bare** clone (the worktree
  source) under `~/.local/share/amux/repos/`. Add with `amux repo add <url|path|OWNER/REPO>`,
  or no-arg to fuzzy-find from your GitHub remotes via `gh`.
- **Workgroup** — a container of agents, in one of two **scopes**:
  - **repo-scoped** — pinned to a single repo, **single-member** (one agent).
    Rendered under **REPOS**, nested beneath its repo header (the container is
    hidden — you just see repo → agents). Auto-created when you start an agent on
    a repo.
  - **work-scoped** — spans any number of repos and holds N agents. Rendered under
    **WORKGROUPS**. Create with the `w` form (name, repos, a baseline description,
    and an optional **Linear** issue woven into the first agent's prompt).
- **Agent** — one agent (Claude) with its own subdirectory and a git **worktree
  per repo** it works on. An agent appears under WORKGROUPS *or* REPOS, never both;
  `m` moves one into a new work-scoped workgroup (with a confirmation dialog).
- **Tabs** — every agent has a row of tabs, switched with **Alt+1/2/3**:
  **1** the agent (Claude), **2** an editor (`$AMUX_EDITOR`, default `nvim`),
  **3** a terminal. **All three are scoped to the agent's worktree** with a
  bubblewrap mount namespace: the system is read-only, the rest of your home —
  other projects, your files, secrets — is replaced by an empty tmpfs, and the
  amux data tree (`~/.local/share/amux`) is mounted **read-only** so git can read
  the bare clone its worktree is sourced from. Writable: the agent's **worktree**
  (to edit) and **its repo's bare clone** (so git can commit to its branch). Only
  what each tool needs is bound back (Claude's config/auth + status hook, the
  editor's config, the shell's rc/theme — e.g. `~/.zshrc` + oh-my-zsh — so the
  terminal keeps your prompt/aliases/plugins, and your **git/GitHub auth**
  (`~/.gitconfig` + `~/.config/gh`) so agents push and use `gh` without logging in
  again). Network works (DNS included). It's a filesystem scope, not a hardened
  jail (network/pids are shared); `AMUX_JAIL=off` disables it.
- **Archive** — `x` marks an agent (or workgroup) done/archived: it drops into a
  collapsed **ARCHIVED** section and its session is stopped. Reversible (`x` again,
  or `amux wg unarchive <id>`).
- **Mode** — an agent runs as a **task** (short) or **loop** (long/autonomous),
  shown with a glyph and exported as `$AMUX_MODE` for your launch wrapper.
- **Control console** (`⚙`) — a built-in agent scoped to amux config + CLI only.

## Philosophy: amux is a UI/orchestration layer, not an autonomy policy

amux switches, displays, launches, and routes — it does **not** decide how an
agent behaves. Autonomy (auto-accept, looping, retries) belongs to the **agent**
or your **launch wrapper**. amux exports intent and gets out of the way:

| env var            | meaning                                              |
|--------------------|------------------------------------------------------|
| `AMUX_MODE`        | `task` or `loop`                                     |
| `AMUX_WORKGROUP`   | the agent's workgroup id (`AMUX_WORKSPACE` = alias)  |
| `AMUX_ROOT`        | the root/workgroup id                                |
| `AMUX_SCOPE`       | `work` or `repo`                                     |
| `AMUX_AGENT`       | the agent kind (`claude`)                            |

Override the launch binary with `AMUX_CLAUDE_BIN` (point it at a wrapper that
branches on `$AMUX_MODE`). Agents launch **pre-trusted** and with
`--permission-mode auto` (a safe classifier, *not* `--dangerously-skip-permissions`);
override with `AMUX_PERMISSION_MODE`. See `scripts/claude-launch.example.sh`.

## Client/server usage

```sh
amux                       # native TUI (default; local)
amux serve                 # run the multiplexer server (local unix socket)
amux serve tcp:0.0.0.0:7077  # also accept remote UIs over TCP
amux harness               # run an agent harness over stdio (remote/decoupled)
AMUX_SERVER=host:7077 amux # point a UI at a remote server  (planned TUI wiring)
```

## Keys (native TUI)

Navigation is **Alt/Option-only** (no prefix):

| key | action |
|-----|--------|
| `↑/↓`, `k/j` | move the rail cursor |
| `Enter` | open the selected agent (or a repo's first/new agent) |
| `Alt+l` / `Alt+h` | focus the agent pane / the rail |
| `Alt+a` | toggle focus between rail and agent |
| `Alt+1/2/3` | switch the agent's tab (agent / editor / terminal) |
| `a` | add an agent — on a repo, a repo-scoped agent; on a workgroup, another agent (settings form) |
| `w` | new work-scoped workgroup (settings form, optional Linear) |
| `R` | track a new repo — GitHub `owner/name`, a git URL, or a local path (form) |
| `m` | move the selected agent to a new workgroup (confirm) |
| `r` | rename the selected agent/workgroup (display name) |
| `x` | archive / restore the selected agent |
| `D` | permanently delete the selected agent/workgroup — worktrees + branch (confirm) |
| `Ctrl+r` | force a state refresh (the daemon also auto-polls) |
| `q` / `Alt+q` | quit |

## Commands

```
amux                       # native TUI
amux serve [listen...]     # multiplexer server (unix + optional tcp:/unix: specs)
amux harness               # agent harness over stdio
amux repo add <src>        # track a repo: git URL | local path | OWNER/REPO (gh)
amux repo ls | rm <name>   # list / untrack repos (rm refuses if agents use it)
amux workgroup repo <repo> # start a repo-scoped agent (alias: wg)
amux workgroup new         # work-scoped workgroup config page
amux workgroup move <agent> [<root>|--new]
amux workgroup archive | unarchive <id>
amux workgroup open|rm|rename|ls
amux console               # open the control console
amux status                # print rail state as text
amux up | dash             # legacy tmux rail / dashboard
```

## Platform support

amux is developed and run on **WSL2** (the day-to-day environment). Everything
else is either build-verified (`make cross` compiles linux+darwin, amd64+arm64)
or relies on a shared, platform-agnostic code path — but has **not been run and
exercised on that platform**. The matrix marks the difference explicitly: ✅ is
something we have actually used here; ⚠️ is something we *expect* to work but have
**not directly validated**.

Two things are OS-specific by design:

- **The filesystem jail is Linux-only.** It shells out to `bwrap` (bubblewrap),
  which exists on Linux/WSL but not macOS. Where `bwrap` is absent the scope is
  **silently skipped** — panes still run, just unscoped to the worktree
  (`AMUX_JAIL=off` is the explicit form). Docker-in-the-pane and `proctree`
  process mapping are likewise Linux-only.
- **tmux is only for the legacy rail** (`amux up` / `dash`). The default native
  TUI hosts every pane in-process over a PTY and needs no tmux at all.

| Capability | Win (WSL) | Win (WSL+tmux) | Mac (iTerm2) | Mac (iTerm2+tmux) | Linux |
|------------|:---------:|:--------------:|:------------:|:-----------------:|:-----:|
| Native TUI (`amux`) | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ |
| Client/server (`amux serve` / `harness`) | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ |
| Worktrees + bare-clone store | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ |
| Agent / editor / terminal tabs | ✅ | ✅ | ⚠️ | ⚠️ | ⚠️ |
| Filesystem jail (`bwrap`) | ✅ | ✅ | ➖ | ➖ | ⚠️ |
| Docker inside the terminal pane | ✅ | ✅ | ⚠️¹ | ⚠️¹ | ⚠️ |
| Process-tree mapping (`proctree`) | ✅ | ✅ | ➖ | ➖ | ⚠️ |
| Legacy tmux rail (`amux up` / `dash`) | ❌² | ⚠️ | ❌² | ⚠️ | ⚠️ |

**Legend** — ✅ run & exercised here (WSL2)  ·  ⚠️ expected to work (shared code
path / build-verified) but **not directly validated**  ·  ➖ not applicable
(degrades gracefully — pane runs unjailed)  ·  ❌ unavailable.

¹ Would run unjailed (no `bwrap` on macOS) and needs Docker Desktop — untested.
² The legacy rail requires tmux; without it, use the default native TUI.

### Notes on testing

Be clear-eyed about what the ✅ / ⚠️ above actually mean:

- **Directly validated (WSL2 only).** The native TUI, git worktrees, the
  `bwrap` filesystem jail, docker-inside-the-terminal-pane, and the
  agent/editor/terminal tabs have all been run and used on WSL2 (recent commits
  fix WSL2-specific resolv.conf/docker paths). The Go test suite — `go test
  ./...`, including the `internal/mux` end-to-end client/server test — passes; it
  runs **with the jail disabled** (`AMUX_JAIL=off`), so it does not cover the
  `bwrap` path.
- **macOS: never executed.** It is only **cross-compiled** (`make cross`),
  never launched. No TUI, pane, git, or tmux behavior has been observed on a
  Mac. The jail (`bwrap`) does not exist there, so panes would run **unscoped**;
  this fallback is in the code but unverified on macOS.
- **Bare/native Linux: not directly validated.** We develop on WSL2, which is a
  Linux kernel but differs in meaningful ways (e.g. `/etc/resolv.conf`
  symlinking, networking, Docker integration). The code paths are shared, so a
  desktop/server Linux install *should* behave like WSL — but we have not run it
  there.
- **Legacy tmux rail (`amux up` / `dash`): not currently exercised.** It is kept
  compiling alongside the native TUI but is not part of the day-to-day flow and
  has not been re-validated against the current code on any platform.

If you run amux on macOS or native Linux and confirm a row, please send the
result so we can promote a ⚠️ to ✅ (or file what broke).

## Install

Requires Go 1.24+, tmux 3.x (only for the legacy `amux up`/`dash` rail), and
(for the jailed terminal, Linux/WSL only) `bwrap`.

```sh
make install
```

Builds `~/.local/bin/amux`, writes the isolated tmux config, and installs the
shell shim. Run `amux` from a normal terminal. (`AMUX_SKIP=1` disables any
auto-launch shim.)

## Roadmap

- **Thin-client TUI** — migrate the native TUI onto `muxclient` so the default app
  is a client of `amux serve` (vterm fed by the server, keystrokes forwarded), and
  it can attach to remote servers.
- **Remote auth/TLS** — v1 TCP is unauthenticated; bind it to trusted networks.
- **Linear** — currently the issue URL is woven into the agent's prompt; a real
  Linear API/sync is a follow-up.
- **One agent kind** (Claude) wired today; the resolver is ready for others.
- Upstream pulls into the bare-clone store; per-repo branch selection.
