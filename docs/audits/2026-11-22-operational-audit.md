# Operational audit — agent-multiplexer — 2026-11-22

Audited commit: `2922c17` (branch `master`). Findings only; no code was changed.

## Scope and method

The audit covered the daemon, agent orchestration, runtime supervision, the
remote provider, per-session host preparation and sandboxing, the direct-access
server, the terminal UI and CLI, and the CI pipeline.

**Client apps.** This repository contains no web, desktop or Android client
code. Those clients talk to the host through the direct-access server
(`internal/direct`), whose contract and fixtures live in `docs/direct/`. The
audit covers that host side; client-side UX in those apps could not be audited
from here.

**How it was done.** The code was split into six areas, each read by a separate
reviewer pass. Every candidate finding was then re-checked against the source
at the cited lines before being included here; a few were reproduced live.

| Check | Result |
|---|---|
| `go build ./...` | passes |
| `go vet ./...` | passes |
| `gofmt -l .` | clean |
| `go test ./...` in `harnessproto` | passes |
| `go test ./...` in the root module, run inside a macOS amux agent sandbox | 42 packages pass, 9 fail (see below) |
| CI on `master` at the audited commit | **failing** (see C-04) |
| Live reproduction inside a macOS amux agent sandbox | P-01, P-02 and P-12 reproduced |

**About the local test failures.** The root-module tests were run from inside an
amux agent sandbox, which is not an environment the suite targets. The failures
that were examined all trace to that environment, not to the code under test:

- Unix-socket tests fail with `bind: invalid argument` because the sandbox's
  `TMPDIR` is 75 characters long. This is P-12 seen directly.
- Tests in `internal/store` and elsewhere fail with `operation not permitted`
  opening the real `~/.config/amux/config.json`, which the sandbox denies (see
  C-05).
- `TestAgentCommandDoesNotWriteSessionControlledGitExclude` fails because the
  sandbox's symlinked `git` cannot find its templates. It passes with the real
  `git` first on `PATH`. This is P-01 again.
- Seatbelt tests cannot nest inside an existing Seatbelt sandbox.

Not every one of the roughly 85 failing tests was examined individually, so
this run is not evidence that the suite is clean. The authoritative signal is
CI, which is covered in C-04.

**Confidence labels**

- **High** — the code path was traced end to end, or the behaviour was reproduced.
- **Medium** — the path was traced, but the trigger was not demonstrated, or the
  outcome depends on an external program's behaviour that was not exercised.
- **Low** — plausible, with a named gap in what could be confirmed.

Nothing here was found by running the system under fault injection. Unless a
finding says "reproduced", it is a reading of the code.

**Finding IDs** are prefixed by area: `D` daemon core, `O` orchestration,
`R` runtime supervision and provider, `P` host preparation and sandbox,
`X` direct access, config and secrets, `T` terminal UI and CLI, `C` CI and build.

## Headline

No critical findings. The ones most likely to hurt in normal use:

- **P-01** — on a Mac with Command Line Tools but no full Xcode, `git fetch` and
  `git push` over HTTPS fail inside every agent sandbox. Reproduced; it broke
  this audit's own first fetch.
- **P-02** — each macOS session's tool directory and its whole `TMPDIR` are
  never deleted. Measured at 1.7 GB for one session.
- **D-01** — the list of agents to relaunch after a daemon restart is
  overwritten with an empty list before the relaunch runs, so any agent that
  fails to come back on the first try is forgotten.
- **O-01** — a routine `prompt` sent to a Claude agent that is sitting on a
  permission dialog is typed into that dialog, and its Enter approves it.
- **R-02** — when a Codex App Server dies mid-turn, the turn is reported as
  having succeeded and nothing is logged.
- **R-01** — one remote-provider pane that stops reading input can wedge the
  whole provider connection while its status still reads "registered".

## Summary

| ID | Severity | Area | Finding | Confidence |
|---|---|---|---|---|
| P-01 | high | sandbox (macOS) | `git` alias is a symlink, so Apple Git cannot find its HTTPS helper | High (reproduced) |
| P-02 | high | sandbox (macOS) | Per-session tool directory and `TMPDIR` are never deleted | High (reproduced) |
| D-01 | high | restart recovery | Restart journal is overwritten before restore runs; restore is one-shot | High |
| O-01 | high | steering | A prompt sent during an open Claude permission dialog answers it | Medium |
| R-01 | high | provider | Pane input is written to the PTY on the read loop; one stuck pane wedges the provider | High |
| R-02 | high | Codex supervision | App Server death mid-turn is reported as success and leaves no trace | High |
| D-02 | medium | session RPC | Failed mailbox opens retry and log every 50 ms with no backoff | High |
| D-03 | medium | locking / shutdown | `amux daemon stop` can wait on the exclusive gate indefinitely and silently | High / Medium |
| D-04 | medium | poll loop | One failed poll publishes an empty session list | High |
| D-05 | medium | serve loop | Any `Accept` error shuts the daemon and every agent down | High |
| D-06 | medium | crash containment | Host-facing goroutines have no panic recovery, and nothing supervises the daemon | High / Low |
| D-07 | medium | restart recovery (macOS) | Codex App Servers can outlive an unclean daemon exit | Medium |
| O-02 | medium | PR watcher | Codex "between turns" check is a 45-second file-age heuristic; failed wake-ups are never retried | Medium |
| O-03 | medium | PR watcher | One failing repo slows polling for every workgroup to 30 minutes | High |
| O-04 | medium | steering / goals | An idle Claude session reads as busy: `stop` can exit it, and goal resume does nothing | Medium |
| O-05 | medium | PR watcher | A lost end-of-turn report leaves a coordinator unwakeable with no log line | Medium |
| O-06 | medium | PR watcher | Queued wake-ups are lost on daemon restart | High |
| R-03 | medium | provider | Lifecycle verbs run on the read loop with no deadline | High / Medium |
| R-04 | medium | provider | A blocked write defeats liveness detection; the register handshake has no deadline | High / Medium |
| R-05 | medium | Codex supervision | Each App Server launch leaks a goroutine and the whole supervisor until daemon exit | High |
| R-06 | medium | Codex supervision | Per-session Codex files are never removed, and session IDs can be reused | High / Medium |
| R-07 | medium | Codex supervision | A failed approval reply leaves the approval unanswerable | Medium |
| R-08 | medium | Codex supervision | One failed event-log write silently disables the log for good | High |
| P-03 | medium | git object pool | Pool generations are never collected, and a too-deep chain permanently blocks the repo | High / Medium |
| P-04 | medium | session creation | A failed agent creation leaves directories and checkouts behind | High |
| P-05 | medium | session creation | Creation fails whenever the remote is unreachable, even with a cached pool | High / Medium |
| P-06 | medium | config drift | Drift scan errors are swallowed, and `amux sandbox drift` prints a false all-clear | High |
| P-07 | medium | removal | A failed removal leaves a session half-deleted; staging leftovers are never swept | High / Medium |
| P-08 | medium | upgrade | The sandbox launcher is re-read from the binary's path at every launch | Medium |
| T-01 | medium | TUI | Terminal-emulator goroutines have no panic recovery; a panic leaves the terminal unusable | High / Medium |
| T-02 | medium | TUI | `x` archives and stops an agent or whole workgroup with no confirmation | High |
| T-03 | medium | TUI | Form input is discarded before the action succeeds; errors are easily lost | High |
| T-04 | medium | TUI / CLI | No client–daemon protocol check outside `amux version` and `amux doctor` | High |
| T-05 | medium | TUI | Pasting into a pane drops bracketed-paste markers | High |
| T-06 | medium | CLI | Host CLI commands wait for the daemon with no deadline | Medium |
| X-01 | medium | direct access | One uncertain grant-store write locks every client out until a manual restart | High / Medium |
| X-02 | medium | direct access | The HTTPS server has no read or write timeouts and reads bodies before authenticating | High |
| X-03 | medium | direct access | Shutdown is not graceful, and closing ends leases with the "revoked" cause | High / Low |
| X-04 | medium | accounts | Account bindings are never removed, and a size cap turns growth into a launch failure | High / Medium |
| C-01 | medium | CI | Tests never run under the race detector | High |
| C-04 | medium | CI | CI on `master` has been red since 2026-11-17 from a timing-sensitive test | High / Medium |
| O-07 | low | console guide | The console guide teaches two commands the daemon always refuses | High |
| O-08 | low | observability | Wake deferral logs every 20 seconds for up to two hours | High |
| O-09 | low | status | The rail shows "goal waiting" while the coordinator is working | High |
| O-10 | low | archive guard | Archive guard and PR watcher disagree on which PRs a member owns | High / Medium |
| O-11 | low | subprocesses | `gh` subprocesses can outlive their timeout | Low |
| O-12 | low | goals | The goal file is rewritten by two processes with no lock | Medium |
| D-08 | low | startup | A second daemon process touches the pidfile and log before taking the lock | High / Medium |
| D-09 | low | logging | A failed reopen during log rotation disables rotation for good | High |
| D-10 | low | shutdown | The client's stop budget is shorter than the daemon's worst-case teardown | Medium |
| D-11 | low | state files | Per-session state files are never cleaned up; some are world-readable | High / Low |
| R-09 | low | process control | Provider panes and App Servers are killed without a grace period | High |
| R-10 | low | provider | Reconnect backoff resets after any accepted registration | High |
| R-11 | low | various | Smaller supervision items | Medium |
| P-09 | low | config homes | A partly seeded config home is treated as complete; a lost manifest re-baselines silently | High / Medium |
| P-10 | low | config homes | The drift scan re-reads every config file of every agent every 10 seconds | High |
| P-11 | low | config writers | A corrupt Claude config file is overwritten instead of reported | High / Medium |
| P-12 | low | sandbox (macOS) | `TMPDIR` is long enough to break Unix-socket paths | High (reproduced) |
| X-05 | low | access | Access credential records are never pruned, and a size cap stops issuance | High / Medium |
| X-06 | low | direct access (web) | The listener speaks HTTP/1.1 only, so browser clients can exhaust their connections | Medium |
| X-07 | low | direct access | History returns 200 with an empty page when the transcript cannot be read | High |
| X-08 | low | direct access | Wake failures are silent, and wake bookkeeping grows without bound | High / Medium |
| X-09 | low | direct access | The grant store is rewritten in full under the lock that gates every request | High |
| X-10 | low | config | Config writes are not durable or not atomic; two sections ignore unknown keys | High / Medium |
| X-11 | low | secrets | Certificate and token expiry has no warning outside tailnet-managed mode | High |
| T-07 | low | TUI | Keybinding configuration errors are never seen | High |
| T-08 | low | TUI | Visited panes are never detached | High |
| T-09 | low | CLI | `amux doctor` can report a broken dependency as OK | High / Medium |
| T-10 | low | legacy relay | A second `amux serve` silently takes over the first one's socket | High |
| T-11 | low | legacy relay | Slow actions block a relay client's input and are reported as failed | High / Medium |
| T-12 | low | CLI | Bare `amux` has no terminal check | High / Low |
| T-13 | low | CLI | Create commands exit 0 when the agent failed to start | High |
| C-02 | low | CI | Jobs have no timeout, concurrency group or token scope; actions are pinned by tag | High |
| C-03 | low | build | Every build reports version `0.1.0`; there is no release pipeline | High |
| C-05 | low | tests | Tests read the developer's real config file and depend on a short `TMPDIR` | High |

Where two confidence levels are given, the first is for the mechanism in the
code and the second is for how readily it triggers.

---

## Sandbox and host preparation

### P-01 — `git` alias is a symlink, so Apple Git cannot find its HTTPS helper

- **Severity:** high
- **Area:** sandbox (macOS)
- **Confidence:** High (reproduced)

**Evidence**

- `internal/panespec/native_git.go:61-63` publishes `git` into the session's tool
  directory through `publishNativeAlias`.
- `internal/panespec/native_github.go:25` — that alias is an `os.Symlink`.
- Apple's Git locates its helper programs relative to the path it was invoked
  by, not the symlink target. Invoked through the alias it cannot compute that
  path and falls back to a compiled-in default under `/Applications/Xcode.app`.
- Nothing sets `GIT_EXEC_PATH` (no match in `internal/` or `cmd/`), and the
  launch environment allow-list would not pass it through:
  `internal/launchenv/launchenv.go:105` admits only `GH_CONFIG_DIR`, `GH_HOST`
  and `GIT_CONFIG_GLOBAL` from that family.
- Reproduced in this audit's own sandbox, on a host with Command Line Tools and
  no `/Applications/Xcode.app`:

  ```
  $ readlink <state>/tools/<hash>/git
  /Library/Developer/CommandLineTools/usr/bin/git
  $ GIT_TRACE=1 git --exec-path
  RUNTIME_PREFIX requested, but prefix computation failed.
  Using static fallback '/Applications/Xcode.app/Contents/Developer/usr'.
  $ git fetch origin
  git: 'remote-https' is not a git command. See 'git --help'.
  ```

**Impact.** Every `git fetch`, `git push` and `git clone` over HTTPS fails inside
agent sandboxes on such hosts. HTTPS is what GitHub-slug repositories get
(`internal/wsops/repo.go:171`), and the agent guide tells every agent to run
`git fetch origin && git merge` before starting. Credential helpers and other
helper programs are lost the same way. On hosts with full Xcode in the default
location the fallback path happens to exist, which is why the macOS CI runners
do not catch this.

**Trigger.** macOS where `xcode-select -p` prints
`/Library/Developer/CommandLineTools`. Open any agent and run
`git ls-remote https://github.com/git/git HEAD`.

**Suggested fix.** Resolve the real exec path once on the host
(`<target> --exec-path`) and export `GIT_EXEC_PATH` in the pane environment, or
publish a small exec wrapper instead of a symlink. Add a test asserting that
`git --exec-path` inside a pane names an existing directory.

---

### P-02 — Per-session tool directory and `TMPDIR` are never deleted

- **Severity:** high
- **Area:** sandbox (macOS)
- **Confidence:** High (reproduced)

**Evidence**

- `internal/core/paths.go:182-185` — on macOS each session gets
  `<state>/tools/<hash>/amux`, a private copy of the binary (18 MB here).
- `internal/panespec/seatbelt.go:84-86` and `:95-96` — the session's `TMPDIR` and
  `CLAUDE_CODE_TMPDIR` are `<state>/tools/<hash>/tmp`.
- The only non-test references to `SessionBinPath` and `nativeTempDir` are their
  definitions and the launch code; nothing removes the directory.
- `internal/wsops/wsops.go:891-916` — `removeAgent` removes checkouts, the config
  manifest and the agent directory, and nothing under `tools/`.
- Measured in this audit's own session: `tmp` is 1.7 GB (build and module caches
  from this audit's test runs).

**Impact.** Every session ever created on macOS leaves its binary copy and
everything its agent wrote to `$TMPDIR` on disk after archive and after delete.
Disk use under the state directory grows without bound.

**Trigger.** Create and delete an agent on macOS; `ls ~/.local/state/amux/tools`
still shows its directory.

**Suggested fix.** Remove the session's tool directory in `removeAgent`. Clear
`tmp` on archive. Sweep directories with no matching session at daemon start,
and report their size in `amux doctor`.

---

### P-03 — Pool generations are never collected, and a too-deep chain permanently blocks the repo

- **Severity:** medium
- **Area:** git object pool
- **Confidence:** High on the code; Medium on how soon the limits are reached

**Evidence**

- `internal/git/pool.go:585-586` — walking the generation chain fails with
  `Git pool generation chain is cyclic or too deep` beyond 1024 entries.
- Each observed upstream advance at session creation appends a generation, and
  there is no compaction. `docs/sandbox-config.md` documents the pool as
  append-only by design.
- `internal/git/pool.go:697-703` — the new tip is written to `current.json`
  **before** `mountClosure` validates the chain, so once the chain is too deep
  the stored tip is the one that fails, on every later call.
- `internal/wsops/repo.go:96` — removing a repo deletes only its git directory;
  the pool stays.
- Each generation becomes one read-only mount or Seatbelt grant in every
  session's launch arguments.

**Impact.** Disk grows forever, including after a repo is removed. A
long-lived, busy repo eventually hits a state where no session can be created
for it until someone edits the pool by hand. The macOS sandbox policy is passed
as a command-line argument and should exceed the argument-size limit well
before 1024 generations; that threshold is an estimate and was not measured.

**Trigger.** Roughly 1025 session creations that each observe a new upstream
head.

**Suggested fix.** Validate the chain before writing `current.json`. Start a
fresh root generation past a small depth. Collect generations no session
references, and delete the pool when the repo is removed.

---

### P-04 — A failed agent creation leaves directories and checkouts behind

- **Severity:** medium
- **Area:** session creation
- **Confidence:** High

**Evidence**

- `internal/wsops/wsops.go:248-254` — after the agent directory is reserved, a
  failed `checkoutRequest` or `git.AddCheckout` returns the bare error.
- `wsops.go:271-279` — the same for a failed guide write or config-home seed.
- `wsops.go:281-290` — cleanup (`removePreparedAgent`) runs only when
  `CreateSession` reports `ErrRootChanged` or `ErrSessionExists`.

**Impact.** A creation that fails on its second repository leaves the first
repository's checkout, its private git directory and its layout record on disk
with no session row. `amux doctor` reports the untracked directory but nothing
reclaims it.

**Trigger.** Create an agent on two repositories with the second one's remote
unreachable.

**Suggested fix.** Defer a cleanup that removes what was prepared unless the
session row was inserted.

---

### P-05 — Creation fails whenever the remote is unreachable, even with a cached pool

- **Severity:** medium
- **Area:** session creation
- **Confidence:** High on the missing fallback; Medium on the lock and timeout impact

**Evidence**

- `internal/git/pool.go:99-104` — a cached pool is trusted for 30 seconds; after
  that a remote round trip is mandatory.
- `internal/git/pool.go:636-645` — `ensureObjectPool` takes the per-repo mutex,
  calls `remoteHead`, and returns its error. There is no fallback to the
  existing `current.json`.
- The pool's `ls-remote` and `fetch` have no timeout of their own; they rely on
  the caller's context, and the mutex is held across the network call.

**Impact.** During a GitHub outage, or on a laptop that is offline, no new agent
can be created on a repository that is already fully cached. One stalled fetch
blocks every other creation for the same repository.

**Trigger.** Disconnect the network and create an agent on a tracked repo.

**Suggested fix.** On a failed remote check with a valid cached pool, use the
cache and log a warning. Give `ls-remote` a short timeout of its own and do not
hold the mutex across it.

---

### P-06 — Drift scan errors are swallowed, and `amux sandbox drift` prints a false all-clear

- **Severity:** medium
- **Area:** config drift
- **Confidence:** High

**Evidence**

- `internal/cfghome/cfghome.go:773-776` — in `Summary`, a `Scan` error sets
  `changes = nil` with no log line.
- `Scan` fails when the manifest cannot be read, and when any file in the copy
  has more than one hard link (`internal/hostprep/root.go:278-280`).
- `cmd/amux/sandbox.go:149-153` prints a scan error to stderr and continues;
  `:168-170` then prints
  `no config drift: every agent's config copy matches its template` on stdout
  and returns nil.
- The package's stated guarantee is that a change "never propagates silently and
  never gets lost silently" (`internal/cfghome/cfghome.go:17-19`).

**Impact.** One hard link inside an agent's config copy — from a package
manager, a local `git clone`, or a deliberate `ln` — makes the rail's
config-edit marker disappear for that session, and the CLI reports all clear
with exit code 0.

**Trigger.** In an agent, hard-link any file inside `$CLAUDE_CONFIG_DIR/skills`,
then edit `settings.json`. Nothing is shown.

**Suggested fix.** Show scan errors as their own state and log them. Make the
CLI exit non-zero and never print the all-clear when any scan failed. Report an
unreadable file as a change of its own instead of failing the whole scan.

---

### P-07 — A failed removal leaves a session half-deleted; staging leftovers are never swept

- **Severity:** medium
- **Area:** removal
- **Confidence:** High that leftovers are never swept; Medium on the read-only-directory trigger

**Evidence**

- `internal/git/anchored_unix.go:179-197` — removal renames the tree into a
  staging directory and then calls `os.RemoveAll` on it. `RemoveAll` does not
  change permissions, so a read-only directory inside the tree (a Go module
  cache, for example) makes it fail after the tree has already left the session.
- `internal/wsops/wsops.go:905-908` — `removeAgent` then returns before deleting
  the session row.
- No code sweeps `<state>/git-staging` or the pool's `.generation-*` staging
  directories; their only references are where they are created.

**Impact.** The row survives but its checkout is gone, so the next launch is
refused. A second delete succeeds and orphans the staged tree for good. A
daemon killed during a large first fetch leaves a multi-gigabyte staging
directory.

**Trigger.** Delete an agent whose worktree contains a read-only directory.

**Suggested fix.** On failure, make the tree writable and retry. Sweep both
staging locations at daemon start.

---

### P-08 — The sandbox launcher is re-read from the binary's path at every launch

- **Severity:** medium
- **Area:** upgrade
- **Confidence:** Medium (traced, not reproduced)

**Evidence**

- `internal/panespec/panespec.go:516-523` (Linux) and
  `internal/panespec/seatbelt.go:156-163` (macOS) resolve `os.Executable()` on
  every pane launch and bind or copy whatever is at that path.
- `Makefile:35` installs with `install`, which replaces the file at that path.
- `Makefile:46` (`uninstall`) and `:71` (`clean`) remove it.

**Impact.** After `make install` without a daemon restart, new panes run a
different binary from the daemon. `docs/versioning.md` does tell operators to
restart. After `make uninstall`, `make clean`, or a development build
disappearing, no pane can start until the daemon is restarted, and the error
does not say why.

**Trigger.** Start the daemon from `bin/amux`, run `make clean`, open a pane.

**Suggested fix.** Open the executable once at daemon start and keep the file
descriptor or a private copy for later launches.

---

### P-09 — A partly seeded config home is treated as complete; a lost manifest re-baselines silently

- **Severity:** low
- **Area:** config homes
- **Confidence:** High on the code; Medium on how often it triggers

**Evidence**

- `internal/cfghome/cfghome.go:235-245` — "seeded" means the directory exists,
  and the directory is created before anything is copied (`:366`).
- `cfghome.go:354-364` — when the manifest cannot be read for **any** reason,
  the copy's current content is adopted as the new baseline.
- Manifest writes are not fsynced and use a fixed temporary name.

**Impact.** A seed interrupted by a full disk or a daemon kill leaves the agent
permanently missing settings, skills or plugins. A truncated manifest makes the
agent's existing edits the baseline, so they stop being reported.

**Trigger.** Kill the daemon while it seeds a large config home.

**Suggested fix.** Record seed completion explicitly and re-run missing entries.
Re-baseline only when the manifest is absent, and log when it happens.

---

### P-10 — The drift scan re-reads every config file of every agent every 10 seconds

- **Severity:** low
- **Area:** config homes
- **Confidence:** High on behaviour; impact scales with config size

**Evidence**

- `internal/source/workspace.go:205` builds each rail row with a drift summary.
- `internal/cfghome/cfghome.go:903-908` reads and hashes every file in the copy
  on each scan; only the template side is cached.
- `cfghome.go:766-773` holds one mutex across the whole scan.

**Impact.** On this host one session's templated tree is about 1,000 files and
25 MB. Twenty agents means roughly 500 MB read and hashed every 10 seconds,
serialized inside the rail poll.

**Suggested fix.** Cache copy-side hashes by file identity, size and
modification time, and scan in the background.

---

### P-11 — A corrupt Claude config file is overwritten instead of reported

- **Severity:** low
- **Area:** config writers
- **Confidence:** High on the code; Medium on the trigger

**Evidence**

- `internal/claudecfg/claudecfg.go:573-577` discards the JSON decode error
  (`_ = dec.Decode(&root)`) and `:596` then writes the file back.
- The same pattern is in `InstallHooksInRooted` (`claudecfg.go:800-816`), which
  runs on every launch.

**Impact.** A truncated or hand-broken `.claude.json` or `settings.local.json`
in a session is replaced by one holding only amux's own entries. The rest of
the content is lost and the corruption is hidden.

**Suggested fix.** On a decode error of a non-empty file, skip the write, log
it, and keep a copy.

---

### P-12 — `TMPDIR` is long enough to break Unix-socket paths

- **Severity:** low
- **Area:** sandbox (macOS)
- **Confidence:** High (reproduced)

**Evidence**

- `internal/panespec/seatbelt.go:84-86` — `TMPDIR` is
  `<state>/tools/<32 hex characters>/tmp`, 75 characters on this host.
- macOS limits a Unix-socket path to 104 bytes.
- Reproduced: this repo's own tests, run inside an agent sandbox, fail with
  `listen unix <TMPDIR>/tsapi1569650019/tailscaled.sock: bind: invalid argument`
  (`internal/tailnet/tailnet_test.go:39`). CI avoids the same problem by setting
  `TMPDIR` to `/tmp` (`.github/workflows/ci.yml`, "Short canonical paths fit
  Darwin Unix sockets").

**Impact.** Tools that create sockets under `$TMPDIR/<dir>/<name>` have under 30
bytes left and can fail with "path too long". Longer user names make it worse.

**Suggested fix.** Use a shorter directory key, or a short per-session temporary
root.

---

## Daemon core

### D-01 — Restart journal is overwritten before restore runs; restore is one-shot

- **Severity:** high
- **Area:** restart recovery
- **Confidence:** High (traced; not executed)

**Evidence**

- `internal/daemon/daemon.go:565-573` reads `live-agents.json` into memory at
  startup and starts restore only after the first poll.
- `daemon.go:841-844` — that first poll calls `persistLiveAgents()` and only then
  releases restore.
- `daemon.go:1559-1611` — `persistLiveAgents` writes the engine's current live
  set. At the first poll nothing has been relaunched, so it writes an empty
  list. It does not include the agents still waiting to be restored.
- `daemon.go:1644-1655` — restore makes one attempt per agent. A failure is
  logged and skipped.
- `daemon.go:1602-1607` — the journal is written without fsync.

**Impact.** From about two seconds after start, the journal on disk holds only
agents that have already come back. An agent whose relaunch fails once — a busy
store, a credential error, a sandbox preparation error — is not retried and is
no longer recorded anywhere; the only trace is one log line. A second restart
or a crash while restore is still working through the list loses the rest.

**Trigger.** Run `amux daemon restart` twice in quick succession with many
agents running. Agents not yet relaunched by the first restart stay stopped.

**Suggested fix.** Persist the union of live and still-pending agents until
restore has finished. Keep failed entries and retry with backoff. Do not
overwrite the journal after a failed first poll (see D-04). Fsync before the
rename.

---

### D-02 — Failed mailbox opens retry and log every 50 ms with no backoff

- **Severity:** medium
- **Area:** session RPC
- **Confidence:** High

**Evidence**

- `internal/daemon/session_lifecycle.go:27` — the reconcile loop ticks every
  50 ms.
- `internal/daemon/session_reconcile.go:99-101` — a session with no server is
  always due for another attempt.
- `session_reconcile.go:145-147` and `session_lifecycle.go:143-145` log each
  failure with a plain `log.Printf`, not through the rate limiter used
  elsewhere in the package.

**Impact.** One session that cannot be opened — its directory was deleted, its
git layout fails validation, its credential is stale — produces about 20 log
lines a second, on the order of hundreds of megabytes a day. The log budget
turns over in under a day and earlier diagnostics rotate away. Each attempt
also redoes credential and git validation.

**Trigger.** Delete one agent's worktree directory while its session is not
archived.

**Suggested fix.** Back off per session, log through the rate limiter keyed by
session and error, and show the state on the session row and in `amux doctor`.

---

### D-03 — `amux daemon stop` can wait on the exclusive gate indefinitely and silently

- **Severity:** medium
- **Area:** locking / shutdown
- **Confidence:** High on the unbounded waits; Medium on indefinite starvation in practice

**Evidence**

- `internal/daemon/session_locks.go:224-231` — `lockEffectExclusive` loops on
  `TryLock` with a sleep. It has no deadline and never falls back to a blocking
  lock.
- The shutdown action takes this path (`internal/daemon/actions.go:177-207`);
  `docs/daemon-locking.md` lists it as deliberately exclusive.
- `internal/daemon/client.go:381-397` — the client waits for the reply with no
  deadline, and the CLI's stop budget starts only after that reply.
- Nothing is logged while the waiter waits.

**Impact.** While a long shared holder is active — a large clone during
`add-repo`, for example — `amux daemon stop` and `amux daemon restart` print
nothing and do not return. Other exclusive actions queue behind it. `SIGTERM`
still works because it does not take the gate.

**Trigger.** Run `amux daemon restart` during a slow `add-repo`.

**Suggested fix.** Take shutdown off the gate; it only closes a channel. Give
the exclusive lock a writer-preference fallback after a short wait, and log who
is waiting.

---

### D-04 — One failed poll publishes an empty session list

- **Severity:** medium
- **Area:** poll loop
- **Confidence:** High on the mechanism; Medium on frequency

**Evidence**

- `internal/daemon/daemon.go:816-820` — a source whose `Poll` fails is logged and
  contributes nothing.
- `daemon.go:836-839` — the result replaces `d.sessions` and is broadcast.

**Impact.** A transient store error (a busy database, an I/O error) is shown to
every client as "no sessions". If it is the first poll after a restart, every
agent is judged not restorable, which feeds D-01.

**Trigger.** Hold a write lock on the database for longer than the busy timeout
during a poll.

**Suggested fix.** Keep the last good result per source on error and mark the
snapshot stale.

---

### D-05 — Any `Accept` error shuts the daemon and every agent down

- **Severity:** medium
- **Area:** serve loop
- **Confidence:** High that the error is fatal; Medium on how reachable it is

**Evidence**

- `internal/daemon/daemon.go:588-594` — an `Accept` error returns from `Run`
  unless the context is already cancelled, and returning runs the full
  teardown.
- Nothing restarts the daemon; it is started again only by the next CLI call.

**Impact.** Running out of file descriptors turns into a full outage: every
agent is stopped. The daemon's descriptor use grows with the number of panes
and clients.

**Trigger.** Reach the open-file limit and open one more client connection.

**Suggested fix.** Retry temporary accept errors with capped backoff, as
`net/http` does, and exit only when the listener is closed.

---

### D-06 — Host-facing goroutines have no panic recovery, and nothing supervises the daemon

- **Severity:** medium
- **Area:** crash containment
- **Confidence:** High on the gap; Low that a specific panic occurs today

**Evidence**

- The only `recover()` calls in `internal/daemon` are in the session-RPC
  callbacks (`session_lifecycle.go:431`, `:447`, `:678`, `:684`).
- The host connection handlers, the poll loop, restore, the PR-watch loop and
  the account-limits loop have none.
- Only provider mode installs a system service.

**Impact.** One nil dereference reachable from a client request or a poll kills
the daemon and, with it, every terminal-attached agent mid-turn. Nothing
restarts it, and recovery then depends on the journal in D-01. No such panic
was found; this is a hardening gap.

**Suggested fix.** Recover per connection and per worker iteration, log the
stack, and drop only that connection or iteration. Consider a service unit for
the daemon.

---

### D-07 — Codex App Servers can outlive an unclean daemon exit

- **Severity:** medium
- **Area:** restart recovery (macOS)
- **Confidence:** Medium

**Evidence**

- `internal/codexapp/supervisor.go:398` starts the App Server in its own process
  group with no parent-death link.
- On Linux the sandbox wrapper adds `--die-with-parent`
  (`internal/panespec/panespec.go:490`); the macOS path has no equivalent.
- `supervisor.go:369-371` — the next start removes the old socket file and
  launches a new server. No process ID is recorded or checked.

**Impact.** After the daemon is killed or panics on macOS, the old App Server
may keep running with the session's credentials, unreachable, while the
restarted daemon launches a second one on the same conversation.

**Not confirmed.** Whether `codex app-server` exits by itself when its client
disconnects or its socket file is removed.

**Trigger.** `kill -9` the daemon on macOS with a structured Codex session
running, then look for `codex app-server` processes.

**Suggested fix.** Record the App Server's process group per session and kill a
stale one before starting a new one.

---

### D-08 — A second daemon process touches the pidfile and log before taking the lock

- **Severity:** low
- **Area:** startup
- **Confidence:** High for the pidfile; Medium for log rotation

**Evidence**

- `cmd/amux/main.go:348-349` writes the pidfile and defers its removal.
- `main.go:356-359` starts the log-rotation watcher.
- The singleton lock is taken only inside `daemon.Run` (`main.go:360`).

**Impact.** A second `amux daemon` that loses the lock race overwrites the
pidfile with its own ID and deletes it on exit, leaving the real daemon with
none. If the log is over its size limit at that moment, the loser rotates it
and the real daemon keeps writing to the rotated file, never rotating again.

**Trigger.** Run any auto-starting `amux` command while another daemon is still
starting up.

**Suggested fix.** Write the pidfile and start rotation after the lock is held.

---

### D-09 — A failed reopen during log rotation disables rotation for good

- **Severity:** low
- **Area:** logging
- **Confidence:** High on the code; the trigger is rare

**Evidence**

- `internal/logrotate/logrotate.go:93-98` renames the log and then opens a new
  one. If the open fails, the log path no longer exists.
- `logrotate.go:76-79` — every later check sees the `Stat` error and returns
  without acting.

**Impact.** The daemon writes to `daemon.log.1` without bound, and an operator
tailing `daemon.log` sees nothing.

**Trigger.** Run out of file descriptors or disk at the moment of rotation.

**Suggested fix.** Open the new file before renaming the old one.

---

### D-10 — The client's stop budget is shorter than the daemon's worst-case teardown

- **Severity:** low
- **Area:** shutdown
- **Confidence:** Medium (from constants; matters only when a join times out)

**Evidence**

- `cmd/amux/main.go:227` — the stop budget is the engine's shutdown budget plus
  20 seconds.
- Teardown chains several joins of 15 seconds each
  (`internal/daemon/daemon.go:701`) plus two engine joins, which sum to well
  over that budget if each one expires.

**Impact.** When a teardown step hangs, `amux daemon restart` gives up without
starting a replacement, and the old daemon exits later. The host is left with
no daemon until the next CLI call.

**Suggested fix.** Derive the client budget from the daemon's worst case, or
keep waiting while the status file shows a live stopping daemon.

---

### D-11 — Per-session state files are never cleaned up; some are world-readable

- **Severity:** low
- **Area:** state files
- **Confidence:** High on the missing cleanup; Low on exposure

**Evidence**

- `internal/core/journal.go:84-87` creates the journal directory `0755` and
  journal files `0644`, append-only.
- No non-test code removes a session's journal, hook-state or notice files when
  the session is deleted.
- `cmd/amux/main.go:347-348` creates the state directory `0755` and the pidfile
  `0644`; `internal/daemon/daemon.go:1603` writes `live-agents.json` `0644`.

**Impact.** Slow, unbounded growth under the state directory. On a host whose
home directory other users can traverse, journal text is readable by them.

**Suggested fix.** Delete per-session files on session delete. Create state
directories `0700` and files `0600`.

---

## Orchestration

### O-01 — A prompt sent during an open Claude permission dialog answers it

- **Severity:** high
- **Area:** steering
- **Confidence:** Medium (the code path is fully traced; the keystroke effect rests on the repo's own verified-behaviour comment, not a live run)

**Evidence**

- `internal/agent/keys.go:120` — for Claude, the submit key and the allow key are
  the same: `Submit: keyEnter, ... Allow: keyEnter`.
- `internal/agent/keys.go:90-91` — "A permission prompt renders its first option
  focused ("❯ 1. Yes"), and Enter submits the focused answer".
- `internal/daemon/steer.go:178-188` — for `prompt` and `interject`, the daemon
  waits for boot and then types the payload. There is no check for an open
  permission on this path.
- `internal/access/scope.go:124-131` — `prompt`, `interject` and `stop` are
  allowed in scope, including on oneself. Only the `permission` verb requires a
  request ID, a runtime generation, and a caller other than the session itself.
- The PR-wake path guards exactly this: `internal/daemon/prwatch.go:448-450`
  defers a wake-up while a permission is open.

**Impact.** A coordinator's routine `amux do steer <member> -f verb=prompt ...`
can approve whatever tool call the member is waiting on, with no request ID and
no permission event. A session can also approve its own dialog by having a
background shell send itself an `interject`. Both bypass the rule, stated in
`scope.go:121-123`, that a session never approves itself.

**Trigger.** A Claude member reaches a permission dialog; the coordinator or the
host sends it any prompt.

**Suggested fix.** In the terminal-typed branch, refuse or queue `prompt` and
`interject` while the session has an open permission, and return an error that
names the pending request.

---

### O-02 — Codex "between turns" check is a 45-second file-age heuristic; failed wake-ups are never retried

- **Severity:** medium
- **Area:** PR watcher
- **Confidence:** Medium (daemon side traced; the App Server's response to a turn started mid-turn was not confirmed)

**Evidence**

- `internal/daemon/prwatch.go:451-454` — the wake gate asks the harness for
  activity.
- `internal/agent/codex.go:227-234` — for Codex, with no recorded hook state,
  that is whether the rollout file was written in the last 45 seconds.
- The only writer of hook state is `internal/daemon/session_reports.go:281`, fed
  by Claude hooks.
- `internal/daemon/prwatch.go:388-395` — any error from starting the turn is
  classed as indeterminate, and `internal/prwatch/prwatch.go:762-763` then marks
  the wake-up as handled so it is never retried.

**Impact.** A Codex coordinator in a long tool call that writes nothing for 45
seconds reads as idle, and the watcher starts a turn on top of the running one.
`docs/pr-watch.md` says wake-ups arrive between turns. Separately, any start
error — including one where nothing was sent — discards the wake-up for good,
leaving only a journal note to reconcile by hand.

**Trigger.** A Codex coordinator runs a command for over 45 seconds while a
member's CI turns red.

**Suggested fix.** Gate on the supervisor's own knowledge of whether a turn is
active. Treat errors raised before anything was sent as deferred, not
indeterminate.

---

### O-03 — One failing repo slows polling for every workgroup to 30 minutes

- **Severity:** medium
- **Area:** PR watcher
- **Confidence:** High

**Evidence**

- `internal/prwatch/prwatch.go:313-329` — any repository's failure counts as a
  failure of the whole poll; the success counter resets only when every
  repository succeeds.
- `prwatch.go:228-234` — the delay doubles per failure up to 30 minutes
  (`:50`), from a base of 3 minutes (`:48`).
- `prwatch.go:356-358` — the failure is logged on the first occurrence and then
  every fifth.

**Impact.** One repository the host's `gh` cannot list — deleted, renamed,
access revoked — backs off polling for all repositories and stays there. A PR's
first sighting never triggers a wake-up by design, so a PR that goes red inside
a 30-minute gap produces none for that failure. Nothing appears in
`amux status`.

**Trigger.** Track a repository, then revoke `gh`'s access to it.

**Suggested fix.** Track failures and backoff per repository, and show a
per-repository watch error in status.

---

### O-04 — An idle Claude session reads as busy: `stop` can exit it, and goal resume does nothing

- **Severity:** medium
- **Area:** steering / goals
- **Confidence:** Medium (paths traced; the idle notification mapping rests on the repo's own comments)

**Evidence**

- `internal/claudecfg/claudecfg.go:615` maps Claude's `Notification` hook, which
  also fires for the idle reminder, to the waiting state.
- `internal/agent/claude.go:290-291` treats waiting as busy.
- `internal/daemon/prwatch.go:456-459` acknowledges the ambiguity and resolves
  it, but only on the wake path.
- `internal/daemon/steer.go:155-158` — the `stop` guard refuses only when the
  session is **not** busy.
- `internal/daemon/steer.go:94-96` — goal resume types its prompt only when the
  session is safe; otherwise it returns success having done nothing.

**Impact.** Once the idle reminder has fired, `stop` sends the interrupt key to
an idle prompt, and the code's own comment (`steer.go:151-154`) says a second
press exits Claude Code. Separately, resuming a paused goal on such a
coordinator writes "active" to the goal file and returns ok while the
coordinator sits idle.

**Trigger.** Leave a Claude session idle until its reminder fires, then send
`stop` twice, or resume its goal.

**Suggested fix.** Use the wake path's "really mid-turn" check in both places.

---

### O-05 — A lost end-of-turn report leaves a coordinator unwakeable with no log line

- **Severity:** medium
- **Area:** PR watcher
- **Confidence:** Medium (traced; how often the report is lost was not measured)

**Evidence**

- `cmd/amux/agent_goal.go:224-228` — the Stop hook's activity report is sent
  with its error discarded, bounded at 10 seconds
  (`cmd/amux/session_client.go:98`).
- `internal/daemon/prwatch.go:455-464` — a session recorded as running defers
  the wake-up with an empty reason, and `:249-251` logs only non-empty reasons.

**Impact.** If the daemon takes over 10 seconds to accept that one report, the
coordinator stays recorded as running. Every wake-up is deferred and then
dropped after two hours, and the rail shows it running indefinitely. Nothing
says why.

**Trigger.** A daemon under lock or mailbox pressure on the turn where the
coordinator ran `goal wait`.

**Suggested fix.** When the recorded state is running but the transcript shows
the turn ended some time ago, treat the session as idle. Log the busy deferral,
rate-limited.

---

### O-06 — Queued wake-ups are lost on daemon restart

- **Severity:** medium
- **Area:** PR watcher
- **Confidence:** High

**Evidence**

- `internal/prwatch/prwatch.go:453-461` saves the PR's new state first and only
  then queues the wake-up in an in-memory map.
- After a restart the saved state already matches, so no change is detected
  again.

**Impact.** A wake-up that was waiting for a busy coordinator disappears when
the daemon restarts or is upgraded. `docs/pr-watch.md` says transitions are
reported exactly once across restarts; that holds for the event, not for the
wake-up. A goal coordinator's restart prompt partly covers this by telling it
to re-check what it was waiting on.

**Suggested fix.** Persist the pending queue alongside the PR state.

---

### O-07 — The console guide teaches two commands the daemon always refuses

- **Severity:** low
- **Area:** console guide
- **Confidence:** High

**Evidence**

- `internal/wsops/guide.go:219-221` tells the console agent to use
  `-f verb=permission -f decision=allow|deny` and `amux do archive <id>`.
- `internal/access/scope.go:62` — the archive toggle is host-only.
- `internal/access/scope.go:128-130` — a permission answer requires a request ID
  and runtime generation, which the guide's form omits.

**Impact.** The console agent follows its guide and receives an opaque refusal
code. The coordinator guide already gives the correct forms.

**Suggested fix.** Use `amux do set-archived <id> -f archived=true` and the full
permission form in the console guide.

---

### O-08 — Wake deferral logs every 20 seconds for up to two hours

- **Severity:** low
- **Area:** observability
- **Confidence:** High

**Evidence**

- `internal/daemon/prwatch.go:248-251` logs each deferral.
- It is retried every 20 seconds for up to two hours
  (`internal/prwatch/prwatch.go:54`, `:58`).

**Impact.** About 360 identical lines per workgroup per batch, in the normal
end state where the coordinator has marked its goal complete and the user then
merges. Real watcher errors are buried.

**Suggested fix.** Log when the reason changes, and drop wake-ups immediately
for finished goals.

---

### O-09 — The rail shows "goal waiting" while the coordinator is working

- **Severity:** low
- **Area:** status
- **Confidence:** High (from code)

**Evidence**

- `internal/claudegoal/claudegoal.go:323-326` — the Stop hook flips a waiting
  goal to active and keeps its note.
- `claudegoal.go:198-201` and `:328-339` — later wake and stop handling does not
  clear the note.
- `internal/daemon/prwatch.go:559-560` displays an active goal with a note as
  waiting.

**Impact.** After one `goal wait`, the coordinator reads "goal waiting" through
every later turn until its next goal mark.

**Suggested fix.** Clear the note on wake, or store an explicit flag.

---

### O-10 — Archive guard and PR watcher disagree on which PRs a member owns

- **Severity:** low
- **Area:** archive guard
- **Confidence:** High on the divergence; Medium on the large-repo case

**Evidence**

- `internal/ghpr/ghpr.go:105` accepts head branches `branch`, `branch-…` and
  `branch/…`.
- `internal/prwatch/prwatch.go:374-376` accepts only `branch` and `branch-…`.
- `internal/ghpr/ghpr.go:60-66` lists at most 200 open PRs with no head filter
  and filters afterwards.

**Impact.** A PR from `branch/x` blocks archiving but is never watched. On a
repository with over 200 open PRs, a member's PR can fall outside the page and
the archive guard lets the archive through.

**Suggested fix.** Share one ownership helper and filter by head branch in the
query.

---

### O-11 — `gh` subprocesses can outlive their timeout

- **Severity:** low
- **Area:** subprocesses
- **Confidence:** Low (no such descendant process was shown to exist)

**Evidence**

- `internal/prwatch/gh.go:26-29` and `internal/ghpr/ghpr.go:63-66` run `gh` with
  a context timeout but no `WaitDelay`.
- `internal/codexapp/supervisor.go:946-947` sets `WaitDelay` for this reason.

**Impact.** If a helper started by `gh` keeps the output pipe open after `gh` is
killed, the call blocks past its timeout. In the watcher that stalls the single
poll loop; in the archive guard it happens under session locks.

**Suggested fix.** Set `WaitDelay` on both.

---

### O-12 — The goal file is rewritten by two processes with no lock

- **Severity:** low
- **Area:** goals
- **Confidence:** Medium (the window is milliseconds wide)

**Evidence**

- `internal/daemon/steer.go:744-757` (host control) and
  `cmd/amux/agent_goal.go:236-244` (the Stop hook) each read, change and
  atomically replace the goal file, with nothing serializing them.

**Impact.** A host "pause" that lands between the hook's read and write is
overwritten, and the loop continues after the user was told it paused.

**Suggested fix.** Lock the file, or compare and retry on its update stamp.

---

## Runtime supervision and provider

### R-01 — Pane input is written to the PTY on the read loop; one stuck pane wedges the provider

- **Severity:** high
- **Area:** provider (compute mode)
- **Confidence:** High on the code path; Medium on how often it triggers

**Evidence**

- `internal/provider/provider.go:649-653` — an input frame is written to the
  pane's PTY directly on the connection's read loop.
- `provider.go:460-464` — session teardown cancels, closes the connection and
  then calls `wg.Wait()`, which waits for that read loop.
- The local engine documents and avoids this hazard
  (`internal/engine/local/local.go:354-357`); the provider does not.
- `provider.go:229-231` sets the disconnected status only after the session
  function returns.

**Impact.** While that write blocks, the read loop handles no frames: not the
kill for that pane, not heartbeat replies. The heartbeat then cancels the
session, and teardown waits on the read loop forever because nothing closes the
PTY. The provider neither reconnects nor arms its grace timer, and its status
file still says registered.

**Trigger.** Send more input than the PTY buffer holds (about 4 KiB) to a pane
whose process is stopped or not reading.

**Suggested fix.** Give each pane a bounded input queue and its own writer, as
the local engine does. Kill panes before waiting on the read loop.

---

### R-02 — App Server death mid-turn is reported as success and leaves no trace

- **Severity:** high
- **Area:** Codex supervision
- **Confidence:** High

**Evidence**

- `internal/codexapp/supervisor.go:413-415` discards the process's exit status:
  `_ = cmd.Wait()`.
- `supervisor.go:1056-1059` — the prompt waiter returns nil for anything received
  on `done`, with a comment assuming a completed turn. `done` also carries the
  disconnected and superseded results.
- The supervisor captures the process's stderr but surfaces it only on a
  startup failure.
- `supervisor.go` contains no logging calls at all.

**Impact.** When `codex app-server` crashes or is killed mid-turn, the caller is
told the prompt succeeded, so the daemon journals nothing. The event log ends
with a turn start and no turn end. Any open permission request stays open. The
exit code and stderr are lost.

**Trigger.** Kill the `codex app-server` process during a turn.

**Suggested fix.** Return an error for disconnected and superseded results. On
process exit, record a notice and a synthetic failed turn end carrying the exit
status and the stderr tail, and log it.

---

### R-03 — Lifecycle verbs run on the read loop with no deadline

- **Severity:** medium
- **Area:** provider
- **Confidence:** High on the mechanism; Medium on the trigger

**Evidence**

- `internal/provider/sessions.go:233-237` — the comment states verbs run inline
  on the read loop because "the store operations are quick".
- The call goes to the daemon through a client that waits with no timeout
  (`internal/daemon/client.go:381-397` shows the same pattern).
- `internal/provider/provider.go:775-778` — the heartbeat cancels the session
  after four intervals without a reply, and replies are only read by that same
  loop.

**Impact.** A lifecycle call the daemon answers slowly makes the provider kill
its own healthy connection and cancel the call mid-flight. The cancelled call
may still have taken effect, so the orchestrator's retry can create a duplicate
workgroup or agent. All pane input and kills wait behind it meanwhile.

**Suggested fix.** Run verbs on a per-session worker with an explicit timeout,
and reply with an error on timeout.

---

### R-04 — A blocked write defeats liveness detection; the register handshake has no deadline

- **Severity:** medium
- **Area:** provider
- **Confidence:** High on the code; Medium on the trigger

**Evidence**

- `harnessproto/harnessproto.go:502-505` — one mutex guards connection writes,
  and the write has no deadline.
- `internal/provider/provider.go:775-781` — the heartbeat checks staleness and
  then writes a ping under that same mutex. Once another writer is blocked on a
  full send buffer, the heartbeat blocks too and never checks again.
- The register write and its reply are bounded only by the context.

**Impact.** A connection that silently stops passing traffic while a pane is
streaming output can take the kernel's retransmit timeout — many minutes — to
be noticed, instead of the documented four heartbeat intervals. A broker that
completes TLS and never answers leaves the provider stuck dialing with no log
line.

**Suggested fix.** Set a write deadline per frame, and a deadline on dial, TLS
and registration.

---

### R-05 — Each App Server launch leaks a goroutine and the whole supervisor until daemon exit

- **Severity:** medium
- **Area:** Codex supervision
- **Confidence:** High

**Evidence**

- `internal/codexapp/supervisor.go:447-450` starts a goroutine that waits for
  the start context to end and then closes the supervisor.
- `internal/codexapp/manager.go:324-345` — the start context's cancel function
  is called only on error paths, never after a successful start and never on
  close.

**Impact.** Each launch pins one goroutine, the supervisor, and its event ring
of up to 4,096 events for the daemon's lifetime, even after the session is
closed. Memory grows with every start and restart of a structured Codex
session.

**Suggested fix.** Keep the cancel function on the supervisor and call it on
close.

---

### R-06 — Per-session Codex files are never removed, and session IDs can be reused

- **Severity:** medium
- **Area:** Codex supervision
- **Confidence:** High on the leak; Medium on the reuse scenario

**Evidence**

- `internal/codexapp/manager.go:441-445` defines `Forget` for a session "deleted
  for good". It has no caller outside its own package.
- No non-test code removes a session's Codex event log.
- Session IDs are three random bytes checked only against existing rows
  (`internal/store/store.go:789-805`).

**Impact.** Identity files and event logs outlive deleted sessions, and event
logs take one line per streamed fragment with no rotation. A new Codex session
that happens to reuse a deleted ID would resume the deleted session's
conversation and append to its log.

**Suggested fix.** Call `Forget` on permanent delete and have it remove the
event log. Tie the identity file to the session's creation, not just its ID.

---

### R-07 — A failed approval reply leaves the approval unanswerable

- **Severity:** medium
- **Area:** Codex supervision
- **Confidence:** Medium

**Evidence**

- `internal/codexapp/supervisor.go:1137-1144` marks the approval answered and
  then sends the reply. A send failure is returned without undoing the mark.
- `internal/codexapp/approvals.go:134` — `drainOutstanding` has no non-test
  caller, so approvals are not cleared when a connection is retired.

**Impact.** If the reply cannot be written, a retry is rejected as a duplicate
and the daemon no longer lists the request as open, while the turn keeps
waiting. After a reconnect, approvals raised on the old connection remain
listed.

**Not confirmed.** Whether Codex re-sends pending requests to a reconnected
client.

**Suggested fix.** Revert to pending when the write fails, and clear or re-key
approvals when a connection is retired.

---

### R-08 — One failed event-log write silently disables the log for good

- **Severity:** medium
- **Area:** Codex supervision
- **Confidence:** High

**Evidence**

- `internal/codexapp/supervisor.go:1681-1702` — any failure to create, open or
  write the event log sets a flag, and every later call returns immediately.
  Nothing is logged and the flag is never cleared.

**Impact.** This file is the source remote consumers read a session's events
from. After one transient disk-full or I/O error, they stop receiving that
session's events while the agent keeps working, with no signal anywhere.
`docs/codex-app-server-supervision.md` says logging is best-effort; it does not
say a single failure is permanent.

**Trigger.** Fill the data volume briefly during a turn.

**Suggested fix.** Log the failure once, retry by reopening the file, and record
a notice on failure and recovery.

---

### R-09 — Provider panes and App Servers are killed without a grace period

- **Severity:** low
- **Area:** process control
- **Confidence:** High on the code

**Evidence**

- `internal/provider/pane.go:142-148` closes the PTY and sends `SIGKILL` to one
  process.
- `internal/codexapp/supervisor.go:943-945` sends `SIGTERM` and `SIGKILL` back
  to back with no wait between them.
- The local engine explains why a grace period matters
  (`internal/engine/local/local.go:33-37`): agents flush their transcript on
  `SIGTERM`.

**Impact.** A provider pane that is killed, or that expires its grace, cannot
flush, and background processes in its group are not signalled.

**Suggested fix.** Reuse the engine's terminate, wait, then kill sequence on the
process group.

---

### R-10 — Reconnect backoff resets after any accepted registration

- **Severity:** low
- **Area:** provider
- **Confidence:** High

**Evidence**

- `internal/provider/provider.go:231-233` resets backoff whenever registration
  was accepted, however briefly the connection lasted.

**Impact.** A broker that accepts and immediately drops is redialled about once
a second indefinitely, re-probing every harness binary each time.

**Suggested fix.** Reset only after the connection has stayed healthy for a
minimum time.

---

### R-11 — Smaller supervision items

- **Severity:** low
- **Area:** various
- **Confidence:** Medium

- `internal/provider/provider.go:816-818` — a repeated spawn for a live pane ID
  replaces the map entry without stopping the first process, which is then
  outside every kill path.
- `harnessproto/harnessproto.go:509` — the frame reader has no line-size cap.
- The App Server dial budget is 15 seconds (`internal/codexapp/supervisor.go:49`)
  while the deferred-start path allows 10 (`internal/daemon/steer.go:50`), so a
  slow cold start is cancelled and retried from scratch.

---

## Direct access, config and secrets

### X-01 — One uncertain grant-store write locks every client out until a manual restart

- **Severity:** medium
- **Area:** direct access
- **Confidence:** High on the mechanism; Medium on how often the write fails

**Evidence**

- `internal/direct/store.go:320-333` — a failed directory sync after the rename
  returns `errAmbiguousCommit`.
- `internal/direct/authority.go:136-138` stores that error as `degraded`, and
  `:148-156` then refuses every later issuance. Nothing clears it.
- `internal/direct/server.go:352-358` answers that refusal with the same 503 used
  for a write of unknown outcome: "the presented code or refresh token may
  already be consumed".
- `docs/direct-access.md:501` tells clients not to retry on that 503 and to
  report "reauthorization required".
- The degraded state is read only inside `internal/direct`; `amux direct status`
  still reports the listener as running.

**Impact.** After one such write, every token refresh and every action gets a
503 for the life of the process. Access tokens last at most ten minutes, so
every client drops out and is told its untouched refresh token may be spent.
The only signal is one line in the direct log. On a filesystem where directory
sync always fails, the first write does this.

**Trigger.** One I/O error on the directory sync of the grant store.

**Suggested fix.** Answer the pre-check refusal with a distinct error saying
nothing was consumed. Show the degraded state in `amux direct status`. In
managed mode, exit so the supervisor restarts the listener — a restart is the
documented recovery.

---

### X-02 — The direct HTTPS server has no read or write timeouts, and reads bodies before authenticating

- **Severity:** medium
- **Area:** direct access
- **Confidence:** High

**Evidence**

- `internal/direct/serve.go:89-95` sets `ReadHeaderTimeout` and `IdleTimeout`
  only. There is no `ReadTimeout`, no `WriteTimeout` and no connection limit.
- `internal/direct/server.go:685` reads the request body, and `:691` admits the
  caller afterwards. The token, claim and revoke routes read bodies the same
  way.

**Impact.** Any peer that can reach the listener can send headers and then feed
a body one byte at a time; each connection holds a goroutine and a file
descriptor indefinitely. An authenticated client that stops reading a large
response holds its handler and its lease, which delays a revoke of that grant.
The default private bind limits who can reach the listener.

**Trigger.** Open many TLS connections, send a `POST` with a content length, and
then send one byte a minute.

**Suggested fix.** Set a per-request read deadline before reading any body, a
write deadline for non-streaming responses, and a connection limit.

---

### X-03 — Shutdown is not graceful, and closing ends leases with the "revoked" cause

- **Severity:** medium
- **Area:** direct access
- **Confidence:** High on the missing drain; Low to Medium on how often a client sees "revoked"

**Evidence**

- `internal/direct/serve.go:99-104` calls `Shutdown` from a goroutine nothing
  waits for, and `:108-109` returns as soon as `Serve` does, which is when
  `Shutdown` starts, not when it finishes.
- `internal/direct/authority.go:251-253` — closing the authority cancels every
  lease with `errAuthRevoked`, the same cause a real revocation uses.

**Impact.** The five-second drain does not happen. A request in flight when the
listener restarts loses its response; an action is left recorded as admitted
with no outcome, and a refresh is committed but unanswered, which the contract
treats as "enroll again". In a narrow window a client is told its access was
revoked on an ordinary restart.

**Trigger.** `amux direct disable` then `enable`, or a daemon restart, while a
request is in flight.

**Suggested fix.** Wait for `Shutdown` to return. Cancel leases at close with a
cause that maps to "unavailable".

---

### X-04 — Account bindings are never removed, and a size cap turns growth into a launch failure

- **Severity:** medium
- **Area:** accounts
- **Confidence:** High on the mechanism; Medium on time to trigger

**Evidence**

- No non-test code deletes from the bindings map in `internal/accounts`.
- `internal/accounts/store.go:241-247` reads at most 8 MiB of `accounts.json`. A
  larger file is truncated by the reader, fails to parse, and is reported as
  `account_store_invalid`.

**Impact.** Every session ever created leaves a binding. Once the file passes
8 MiB — on the order of ten thousand sessions, by estimate — every harness
launch fails with an error that does not mention size, and the file needs hand
repair.

**Suggested fix.** Drop a session's binding when the session is deleted, and
report an over-size file as such.

---

### X-05 — Access credential records are never pruned, and a size cap stops issuance

- **Severity:** low
- **Area:** access
- **Confidence:** High on the mechanism; Medium on time to trigger

**Evidence**

- `internal/access/authority.go:696-701` adds a record per issuance and refuses
  when the registry would exceed 4 MiB (`:37`).
- Records are added or marked revoked; no code path removes them. A record is
  added per session creation and per credential rotation.

**Impact.** At the cap, new sessions cannot get a credential and existing ones
stop working when theirs lapses, with "access state capacity exceeded". Nothing
compacts the file.

**Suggested fix.** Prune revoked records for sessions that no longer exist.

---

### X-06 — The listener speaks HTTP/1.1 only, so browser clients can exhaust their connections

- **Severity:** low
- **Area:** direct access (web clients)
- **Confidence:** Medium (the server side is certain; the web client lives in another repository)

**Evidence**

- `internal/direct/serve.go:44` builds the TLS configuration with no ALPN
  protocols, and `:88` wraps the listener by hand, so HTTP/2 is never
  negotiated.
- Transcript streams are long-lived responses, one per session.

**Impact.** A browser allows six HTTP/1.1 connections per origin across all
tabs. Six open transcript streams leave none for actions or token refresh;
those requests queue while access tokens expire.

**Trigger.** Open six tabs of the web client on session transcripts, then send a
prompt.

**Suggested fix.** Offer `h2` in the TLS configuration and confirm the stream
deadlines still behave.

---

### X-07 — History returns 200 with an empty page when the transcript cannot be read

- **Severity:** low
- **Area:** direct access
- **Confidence:** High

**Evidence**

- `internal/direct/server.go:520-532` — when the backend reports failure, the
  handler still writes a 200 with an empty event list. Failure covers a daemon
  error and a read error as well as "no transcript yet".
- The stream route answers the same condition with a "not found" end event.

**Impact.** A client shows an empty transcript and has no reason to retry.
Nothing is logged.

**Suggested fix.** Return an error for real failures and map it to 503; keep the
empty 200 for "no transcript yet".

---

### X-08 — Wake failures are silent, and wake bookkeeping grows without bound

- **Severity:** low
- **Area:** direct access (push wake-ups)
- **Confidence:** High on the code; Medium on impact

**Evidence**

- `internal/direct/wake_service.go:153-156` returns early when the daemon
  snapshot fails, before the code that computes lag, so the service never
  reports itself as lagging.
- Results of flushing the wake store are discarded at every call site in
  `wake_service.go`, `wake_api.go` and `admin.go`.
- Per-session generation entries in the wake store are never deleted.

**Impact.** When the daemon is hung, clients are told wake-ups are enabled and
healthy while none are sent, which contradicts `docs/direct-wake-contract.md`.
An operator has nothing to go on. The wake file is rewritten in full, and it
only grows.

**Suggested fix.** Log flush and snapshot failures, rate-limited. Compute lag
when the snapshot fails. Prune entries for sessions no longer in the inventory.

---

### X-09 — The grant store is rewritten in full under the lock that gates every request

- **Severity:** low
- **Area:** direct access
- **Confidence:** High (the size and cap are documented as accepted)

**Evidence**

- `internal/direct/authority.go:128-144` marshals the whole grant state and
  syncs it twice while holding the authority's mutex, which admission also
  takes.
- `docs/direct-access.md` documents about 620 KB per mature grant and a
  1,024-record cap per grant.

**Impact.** With several long-lived grants, every request waits behind
megabyte-sized synced writes. Separately, over 1,024 actions on one grant in 24
hours returns `capacity_exhausted` with advice to enroll again, although the
records become compactable after 24 hours — a temporary condition presented as
permanent.

**Suggested fix.** Keep per-grant history in separate files, and distinguish
"retry later" from a real cap.

---

### X-10 — Config writes are not durable or not atomic, and two sections ignore unknown keys

- **Severity:** low
- **Area:** config
- **Confidence:** High on the code; Medium on crash-time impact

**Evidence**

- `internal/amuxcfg/config.go:46-58` writes a temporary file and renames it
  without syncing the file or the directory.
- `internal/providercfg/config.go:103` uses `os.WriteFile`, which truncates in
  place.
- The `goal` and `codex` config sections read their known keys and ignore
  others; the `direct` section is strict.

**Impact.** A crash can leave an empty `config.json`, which stops the direct
listener and fails the daemon's Codex control resolution at start. A mistyped
key such as `"auto-resume": false` is silently ignored and leaves the default
in force.

**Suggested fix.** Sync the file and directory, write the provider config
atomically, and reject unknown keys in every section.

---

### X-11 — Certificate and token expiry has no warning outside tailnet-managed mode

- **Severity:** low
- **Area:** secrets
- **Confidence:** High

**Evidence**

- `internal/tailnet/renew.go:207-211` — with an explicit certificate, the
  renewer returns without logging when the files are unchanged.
- A manually started `amux direct serve` loads its certificate once and never
  reloads it.
- The central Claude setup token's one-year lifetime appears only as a line in
  `amux auth status`.

**Impact.** Clients start failing TLS, or agents start getting 401s, with
nothing in the logs beforehand. Only `amux direct status` and `amux doctor`
show an approaching certificate expiry.

**Suggested fix.** Log a warning when under 30 days remain on an explicit
certificate, and when the setup token is about eleven months old.

---

## Terminal UI and CLI

### T-01 — Terminal-emulator goroutines have no panic recovery; a panic leaves the terminal unusable

- **Severity:** medium
- **Area:** TUI
- **Confidence:** High on the gap and its consequence; Medium that a panic occurs in practice

**Evidence**

- `internal/vterm/vterm.go:225-226` starts two goroutines per pane that feed
  agent output through the terminal emulator.
- There is no `recover()` anywhere in `internal/vterm`, `internal/nativetui` or
  `internal/clipboard`.
- The code already works around one panic in the pinned emulator
  (`vterm.go:133-136`): "accepts an out-of-range bottom margin and later panics
  in scroll operations".

**Impact.** A panic in any goroutine ends the process, and the UI framework's
own panic handling cannot restore the terminal for a goroutine it does not own.
The user's shell is left in raw mode on the alternate screen with mouse
reporting on, and needs `reset`. Agents keep running in the daemon. The
emulator parses untrusted output from every open pane, including hidden ones.

**Suggested fix.** Recover in both goroutines, mark the pane closed and show an
error. Install a process-level terminal restore for any fatal exit.

---

### T-02 — `x` archives and stops an agent or whole workgroup with no confirmation

- **Severity:** medium
- **Area:** TUI
- **Confidence:** High

**Evidence**

- `internal/nativetui/nativetui.go:644-651` — `x` on the rail sends the archive
  action immediately, for an agent or a workgroup root.
- `internal/core/core.go:127` describes that action as a toggle, and
  `internal/wsops/wsops.go:1219` implements it as
  `SetArchived(ctx, id, !s.Archived)`.
- Restart, move and delete each ask for confirmation; in those dialogs Enter
  counts as yes (`nativetui.go:479`).

**Impact.** A stray `x` while moving through the rail stops a running agent, or
every agent in a workgroup, mid-turn. Because the action is a toggle computed
from the current state, a second press before the screen refreshes restores the
session — after its process was already stopped.

**Suggested fix.** Send the explicit set-archived action, and confirm when the
target is running or is a workgroup.

---

### T-03 — Form input is discarded before the action succeeds; errors are easily lost

- **Severity:** medium
- **Area:** TUI
- **Confidence:** High

**Evidence**

- `internal/nativetui/form.go:837-841` clears the form and then sends the
  action.
- `internal/nativetui/nativetui.go:704-708` — with no connection the send
  returns "not connected".
- `nativetui.go:287` clears the status line on reconnect, and `:308` overwrites
  it on disconnect.

**Impact.** Submitting a new-workgroup form while the daemon is restarting loses
the typed prompt. A rejection from the daemon also drops the form, and the
error is one line cut to the terminal width that the next reconnect erases. If
the connection drops after the send, nothing says the outcome is unknown.

**Trigger.** Restart the daemon in another terminal, then submit a form in the
TUI inside the reconnect window.

**Suggested fix.** Keep the form until a result arrives and reopen it with the
error on failure. Keep the last action error where reconnects do not clear it.

---

### T-04 — No client–daemon protocol check outside `amux version` and `amux doctor`

- **Severity:** medium
- **Area:** TUI / CLI
- **Confidence:** High that there is no check; the impact depends on the skew

**Evidence**

- The only protocol comparison is in `cmd/amux/version.go`. `internal/nativetui`
  never references the daemon protocol or the version query.
- `cmd/amux/version.go:140` discards the compatibility result, so
  `amux version` exits 0 on a known mismatch.

**Impact.** After installing a new binary without restarting the daemon, the new
TUI and CLI drive the old daemon with no warning. `docs/versioning.md`
documents the restart requirement, but nothing in the dashboard surfaces it.

**Suggested fix.** Query the daemon's version on connect and show a persistent
banner on a mismatch. Refuse mutating CLI actions on a protocol mismatch.

---

### T-05 — Pasting into a pane drops bracketed-paste markers

- **Severity:** medium
- **Area:** TUI
- **Confidence:** High on the code path

**Evidence**

- `internal/nativetui/nativetui.go:569-572` forwards key input to the pane as
  raw bytes.
- The paste flag is consulted only in forms and the accounts screen
  (`internal/nativetui/form.go:620`, `accounts.go:265`).
- `docs/pane-selection.md` records that the markers are not re-emitted.

**Impact.** A multi-line paste into the shell tab runs line by line as it
arrives, because the shell's paste protection never sees the markers. Editors
re-indent pasted text.

**Suggested fix.** When the pane has asked for bracketed paste, wrap pasted
bytes in the markers.

---

### T-06 — Host CLI commands wait for the daemon with no deadline

- **Severity:** medium
- **Area:** CLI
- **Confidence:** Medium (the missing deadline is certain; no stall was reproduced)

**Evidence**

- `cmd/amux/do.go:280-291` loops on the next frame with no timeout. The status
  and query paths have the same shape.
- The in-agent path is bounded at 35 seconds; the host path is not.

**Impact.** `amux status --json`, `amux do ...` and `amux top` never return when
the daemon accepts the connection but does not answer. Scripts hang.

**Suggested fix.** A default deadline on queries and a generous, overridable one
on actions.

---

### T-07 — Keybinding configuration errors are never seen

- **Severity:** low
- **Area:** TUI
- **Confidence:** High

**Evidence**

- `internal/nativetui/nativetui.go:57-59` puts the error in the status line.
- `nativetui.go:287` and `:308` overwrite the status line within milliseconds,
  when the connection attempt resolves.

**Impact.** A bad `keys` entry silently reverts to the default binding.

**Suggested fix.** Hold startup warnings in a field that connecting does not
clear.

---

### T-08 — Visited panes are never detached

- **Severity:** low
- **Area:** TUI
- **Confidence:** High on the code; the impact was not measured

**Evidence**

- `internal/nativetui` never sends a pane-close request; a pane leaves the
  TUI's set only when its process exits or it is restarted.
- Each open pane keeps an emulator and receives every chunk of its output, even
  when hidden.

**Impact.** A dashboard left open for a day after visiting many agents parses
all of their output streams continuously. Each pane's memory is bounded; the
number of panes is not.

**Suggested fix.** Detach hidden panes beyond a small cap. The daemon already
replays on reopen.

---

### T-09 — `amux doctor` can report a broken dependency as OK

- **Severity:** low
- **Area:** CLI
- **Confidence:** High on the code; Medium on the macOS trigger

**Evidence**

- `cmd/amux/doctor.go:99-100` prints a tick for any binary found on the path.
- `doctor.go:534-538` discards the error from running `--version`.
- `doctor.go:526` runs the user's login shell with no timeout.

**Impact.** On macOS without Command Line Tools, `/usr/bin/git` exists and
fails; doctor prints a tick with a blank version and reports all dependencies
present. A slow shell startup file can stall doctor. Doctor also does not check
the condition in P-01.

**Suggested fix.** Treat a failed version check as a failure for required
dependencies, and bound the shell lookup.

---

### T-10 — A second `amux serve` silently takes over the first one's socket

- **Severity:** low
- **Area:** legacy relay
- **Confidence:** High on the code

**Evidence**

- `internal/mux/listen.go:40-45` removes the socket path unconditionally before
  listening. There is no lock and no check that a server is already there.

**Impact.** The first server keeps running unreachable. When it exits, it
removes the path the second server is using.

**Suggested fix.** Dial the socket first and refuse to start if it answers.

---

### T-11 — Slow actions block a relay client's input and are reported as failed

- **Severity:** low
- **Area:** legacy relay
- **Confidence:** High on the blocking; Medium on the slow-link case

**Evidence**

- `internal/mux/server.go:372-377` runs each action inline on the client's read
  loop under a 30-second timeout.
- `server.go:1096` gives every frame a 5-second write deadline, while one pane
  frame can be several megabytes.

**Impact.** While a slow create is in flight, that client's keystrokes queue
unread. A create that takes over 30 seconds is reported as failed although the
daemon completes it. A large frame on a slow link drops the client.

**Suggested fix.** Run actions off the read loop. Do not report a relay timeout
as a definite failure.

---

### T-12 — Bare `amux` has no terminal check

- **Severity:** low
- **Area:** CLI
- **Confidence:** High that there is no check; Low on the exact behaviour without a terminal

**Evidence**

- `cmd/amux/main.go:667-677` cleans up hooks, starts the daemon if needed and
  launches the TUI with no terminal check. The interactive CLI pages do check
  (`cmd/amux/cli.go:144-149`).

**Impact.** `amux | cat`, or an agent running bare `amux` to see usage, starts
the daemon and the TUI instead of printing help.

**Suggested fix.** Print usage and exit 2 when standard input or output is not a
terminal.

---

### T-13 — Create commands exit 0 when the agent failed to start

- **Severity:** low
- **Area:** CLI
- **Confidence:** High (the comment marks it deliberate)

**Evidence**

- `cmd/amux/cli.go:259-263` prints a warning when the start fails, and every
  caller then returns success.

**Impact.** A script running `amux workgroup create ... --prompt ...` cannot tell
that the agent never started.

**Suggested fix.** A distinct exit code, or a `started` field in JSON output.

---

## CI and build

### C-01 — Tests never run under the race detector

- **Severity:** medium
- **Area:** CI
- **Confidence:** High

**Evidence**

- `Makefile:48-49` — `make test` runs `go test ./...` with no `-race`.
- No file under `.github/`, nor the `Makefile` or `CONTRIBUTING.md`, mentions
  `-race`.

**Impact.** The daemon is heavily concurrent — per-session locks, an effect
gate, supervisors, a provider read loop — and several findings above are
concurrency defects (R-07 notes an unsynchronized field read across a
reconnect). Nothing in the pipeline would catch a new data race.

**Suggested fix.** Add a `go test -race ./...` job, at least on Linux.

---

### C-02 — Jobs have no timeout, concurrency group or token scope; actions are pinned by tag

- **Severity:** low
- **Area:** CI
- **Confidence:** High

**Evidence**

- `.github/workflows/ci.yml` contains no `timeout-minutes`, `concurrency` or
  `permissions` key.
- Third-party actions are referenced by tag (`actions/checkout@v4`,
  `actions/setup-go@v5`, `actions/upload-artifact@v4`). Downloaded fixtures, by
  contrast, are checksum-verified.

**Impact.** A hung integration test holds a macOS runner for the default six
hours. Superseded pushes keep running. The workflow token has the repository's
default scope.

**Suggested fix.** Add job timeouts, a concurrency group per ref, a read-only
`permissions` block, and pin actions by commit.

---

### C-03 — Every build reports version `0.1.0`; there is no release pipeline

- **Severity:** low
- **Area:** build
- **Confidence:** High

**Evidence**

- `Makefile:5` sets `VERSION ?= 0.1.0`, and `internal/buildinfo/buildinfo.go`
  defaults to the same value.
- The repository has no tags and no release workflow.

**Impact.** `amux version` cannot distinguish one build from another, which
makes T-04 and the upgrade guidance in `docs/versioning.md` harder to act on.
The commit is recoverable only through `go version -m`.

**Suggested fix.** Embed the commit in the reported version, for example from
`git describe` at build time.

---

### C-04 — CI on `master` has been red since 2026-11-17 from a timing-sensitive test

- **Severity:** medium
- **Area:** CI
- **Confidence:** High that the run failed as described; Medium that the test is flaky rather than broken

**Evidence**

- The latest `ci` run on `master`, for the audited commit `2922c17` (pushed
  2026-11-17), concluded `failure`. The two runs before it on `master`
  succeeded.
- The failing job is `build-test (ubuntu-latest)`, step `test`:

  ```
  --- FAIL: TestMuxPaneResyncRestatesModes (5.93s)
      hardening_test.go:790: mirror never cleared its marker
  FAIL	amux/internal/mux	6.566s
  ```

- `build-test (macos-26)` was cancelled when the Ubuntu job failed, because the
  matrix uses the default fail-fast behaviour. Its result for this commit is
  unknown.
- The same test passed in this audit's local run of `internal/mux`.
- On the pull-request branch whose runs precede that push, 3 of 11 `ci` runs
  failed and the rest passed. Those three failures were not examined, so
  whether they share this cause is unknown.

**Impact.** The default branch has shown a failing check for five days with no
re-run. A red default branch hides the next real regression, and intermittent
failures on pull requests teach people — and the agents this project runs — to
re-run until green.

**Trigger.** Unknown. The assertion waits for a pane mirror to clear a marker
within a time limit, which suggests sensitivity to runner load.

**Suggested fix.** Re-run the job to restore the signal. Make the test wait on
an explicit event instead of a deadline, or raise its limit. Set
`fail-fast: false` so one platform's failure does not hide the other's result.

---

### C-05 — Tests read the developer's real config file and depend on a short `TMPDIR`

- **Severity:** low
- **Area:** tests
- **Confidence:** High (observed)

**Evidence**

- Run inside a sandbox that denies it, tests in `internal/store` fail with
  `open /Users/<user>/.config/amux/config.json: operation not permitted`
  (`store_test.go:95`, `prs_test.go:8`, `workgroups_test.go:21`). So on an
  ordinary machine those tests open the developer's real configuration.
- Socket tests fail when `TMPDIR` is long (see P-12); CI sets it to `/tmp` to
  compensate.

**Impact.** Test results can depend on what is in the developer's own
`config.json`, and the suite cannot be run by an amux agent inside its own
sandbox — which is how this project expects most changes to be made and
verified.

**Suggested fix.** Point the config directory at a temporary directory in test
setup, and have socket tests create their own short directory under `/tmp`.

---

## Checked and found sound

**Daemon core**

- Teardown is one ordered sequence with bounded joins that dump goroutines when
  they expire (`internal/daemon/daemon.go:402-477`).
- The singleton lock and socket handling never remove a live socket, and the CLI
  never signals a process from the pidfile.
- `SIGTERM` takes no lock, so a daemon stuck on the effect gate can still be
  stopped.
- Store migrations run in one transaction, and a database from a newer schema
  is refused before any change (`internal/store/store.go:311-331`).

**Orchestration**

- Session locks are always taken before the creation lock, on every path
  examined.
- `amux agent done` checks for an open PR before archiving, and a restore
  cancels a pending completion.
- Goal loops are bounded at 100 continuations, and a broken hook lets the turn
  end (`internal/claudegoal/claudegoal.go:321-340`).
- A failed PR listing never marks a PR closed, and wake-ups are re-authorized
  at delivery.

**Runtime supervision and provider**

- JSON-RPC writes to the App Server are bounded at 10 seconds, and pending calls
  fail when the connection closes.
- Replay buffers are bounded, and slow subscribers are dropped.
- Harness downloads are checksum-verified, extraction rejects path escapes, and
  installs are staged and renamed.

**Sandbox and host preparation**

- Host-side git never prompts: terminal prompts and askpass are disabled
  (`internal/git/pool.go:209-235`).
- Pool publication is atomic and tolerates concurrent creators.
- Config-home seeding does not copy credentials or follow symlinks.
- Sandbox assembly fails closed: an old `bwrap` is refused, and a credentialed
  launch with the jail off is refused.

**Terminal UI and CLI**

- The UI never blocks on a socket write, and disconnect and reconnect states
  are shown explicitly.
- Clipboard helpers are bounded at 5 seconds and run off the UI goroutine.
- Scrollback and per-pane backlogs are bounded.

**CI**

- The docs-only classifier fails toward running CI when it cannot decide
  (`.github/ci-scope.sh`), and has its own behaviour tests.
- Downloaded harness fixtures are pinned by SHA-256.
