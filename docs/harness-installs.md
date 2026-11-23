# amux-managed harness installs

amux installs its own standalone copies of Claude Code and Codex, separate
from anything you installed globally, and keeps them updated. This is on by
default for the harnesses you already use: the daemon (and `scripts/install.sh`)
adopts a managed install for each harness whose binary is on your PATH or that
you are logged in to, installs new releases as they come out, and moves running
sessions onto the new version when they are idle between turns. It never
installs a harness you don't use, never overrides an explicit binary choice,
and never moves a pinned harness. See [Automatic adoption](#automatic-adoption)
and [Running sessions](#running-sessions-move-to-the-new-version) for the
rules, and [Opting out](#opting-out) to keep the old behaviour (launch whatever
`claude` / `codex` is on your PATH).

```sh
amux harness adopt                 # managed copies of the harnesses you use (what the daemon does)
amux harness adopt --dry-run       # what adoption would do, and why
amux harness install claude        # Claude Code, "stable" channel
amux harness install codex         # Codex, latest release
amux harness ls                    # what each harness launches, installed versions
amux harness update                # update every managed harness
amux harness pin claude 2.1.311    # activate and freeze a version
amux harness unpin claude
amux harness rollback codex        # re-activate the previous version
amux harness check                 # look for a newer release without installing
amux harness gc                    # delete versions nothing uses
amux harness uninstall codex       # go back to the Codex on your PATH (and stop adopting it)
```

## Where the binaries come from

amux downloads the harnesses' **official native builds**, the same files their
own installers use:

| harness | source | integrity check |
|---------|--------|-----------------|
| Claude Code | `https://downloads.claude.ai/claude-code-releases` (`/stable` and `/latest` give the version; `/<version>/manifest.json` lists each platform's SHA-256; `/<version>/<platform>/claude` is the self-contained binary) | SHA-256 and size from the manifest |
| Codex | GitHub releases of `openai/codex`: `codex-package-<target>.tar.gz` (`bin/codex` plus its bundled `rg` and `bwrap` helpers) | SHA-256 from the release's `codex-package_SHA256SUMS` |

After the download is verified, amux runs the new binary's `--version` once. If
that fails, the version is not installed.

**Why native builds instead of npm.** The npm packages
(`@anthropic-ai/claude-code`, `@openai/codex`) need a Node.js runtime. amux
would then depend on whatever Node the user has, and it would have to mount that
Node into every sandboxed pane. The native builds are self-contained, so a
version directory has everything the harness needs. That directory is also the
one thing a pane's sandbox has to mount. Both vendors now treat the native
builds as their primary install path, and both publish checksums for them.

## Layout and switching versions

```
~/.local/share/amux/harnesses/        ($XDG_DATA_HOME/amux/harnesses)
  claude/
    2.1.311/bin/claude                one directory per version, never modified
    2.1.325/bin/claude
    current -> 2.1.325                relative symlink
    state.json                        pinned, previous, last update check
    .lock                             flock serializing installs and switches
  codex/
    0.169.0/bin/codex
    0.169.0/codex-path/rg
    0.169.0/codex-resources/bwrap
    current -> 0.169.0
```

- **Installs go to a staging directory first.** amux downloads into
  `<kind>/.staging-*`, verifies the files, and then renames the staging
  directory to `<kind>/<version>`. An interrupted install leaves no
  half-installed version behind.
- **Switching is atomic.** install, update, pin and rollback create a new link
  and `rename(2)` it over `current`.
- **A launch pins its version.** At launch amux resolves `current` to the
  concrete version directory, so a session's argv names
  `…/claude/2.1.311/bin/claude`. Moving `current` never changes a running
  process; the daemon then restarts running sessions onto the new version at a
  safe point (see [Running sessions](#running-sessions-move-to-the-new-version)).
- **Sandboxing.** A protected pane (bubblewrap on Linux, Seatbelt on macOS)
  mounts only the version directory it runs, read-only. An agent cannot modify
  the managed install and cannot see other versions.
- **Garbage collection.** `amux harness gc` (run automatically after install
  and update) keeps the current version, the previous one (the rollback target)
  and the pinned one. It also keeps any version that a running process is
  executing, found through `/proc/*/exe` on Linux and `ps` on macOS. If amux
  cannot list processes, GC deletes nothing. `uninstall` removes the `current`
  link straight away, so new launches fall back to PATH. It deletes every
  version that isn't running, and leaves running versions for a later `gc`.

## Which binary a launch uses

For each harness, the first of these that is set wins:

1. `AMUX_<KIND>_BIN` in the daemon's environment (`AMUX_CLAUDE_BIN`, `AMUX_CODEX_BIN`).
2. `amux config set harnesses.<kind>.bin <path>`.
3. The amux-managed install (`current`).
4. Your own install: PATH, then your login shell's `command -v`, then the usual
   per-user install locations (nvm, `~/.local/bin`, `~/.claude/local`, bun).

The same order applies everywhere amux runs a harness: agent panes, the Codex
App Server and its `--remote` attach, the host-side central Codex login, and
`amux doctor`. `amux doctor` and `amux harness ls` show which binary is used and
why, for example `(amux-managed 2.1.311)`, `(override AMUX_CLAUDE_BIN)` or
`(PATH)`.

## Updates are owned by amux

In sessions it launches, amux turns off the harnesses' own self-updaters:

- Claude Code gets `DISABLE_AUTOUPDATER=1`.
- Codex gets `-c check_for_update_on_startup=false` (on both the TUI and the
  `--remote` attach).

This applies whichever binary is used. A sandboxed pane cannot write your global
install anyway, so the self-updater could only fail or show prompts you can't
act on. Your own interactive `claude` / `codex` outside amux keeps updating
itself as usual. A managed install is unaffected by that, because it lives in
its own directory.

### Scheduled checks

The daemon checks for updates on a schedule. `auto` is the default:

```sh
amux config set harnesses.update auto      # default: adopt, install + activate (not pinned ones)
amux config set harnesses.update notify    # only record + log available updates
amux config set harnesses.update off       # no checks, no adoption
amux config set harnesses.check_interval 12h   # default 24h, minimum 1h
amux config set harnesses.claude.channel latest  # claude: stable (default) | latest
amux config set harnesses.restart_sessions off   # keep running sessions on their version
```

- The daemon re-reads these settings every 15 minutes (`restart_sessions`
  within a minute). You don't need to restart it.
- A check runs only when the last one is older than the interval. The time of
  the last check is stored in `state.json`, so restarting the daemon doesn't
  trigger a new check.
- Only harnesses with a managed install are checked.
- **notify** records the newest version. `amux harness ls` and `amux doctor`
  then show `update X available`, and the daemon log has a line.
- **auto** also installs and activates the new version, then runs GC. It
  applies the same rules as a manual update: pinned harnesses are skipped.

### Automatic adoption

In `auto` mode the daemon (every 15 minutes) and `scripts/install.sh` (once,
through `amux harness adopt`) install and activate a managed copy of each
harness that passes every one of these rules (`harnessbin.AdoptDecision`):

1. **You already use it**: its binary resolves (PATH, your login shell's
   `command -v`, or the usual per-user install locations), or you are logged in
   to it (`~/.claude/.credentials.json` or the macOS keychain item
   `Claude Code-credentials`; `~/.codex/auth.json`; or amux's shared login
   copies). amux never installs a harness you don't use.
2. **No explicit binary is configured**: `AMUX_<KIND>_BIN` or
   `harnesses.<kind>.bin` win over a managed install, so amux leaves that
   harness alone rather than install something that would never launch.
3. **It is not managed yet.** An existing managed install, pinned or not, only
   ever moves by the update rules; a pin is never moved.
4. **You did not uninstall it.** `amux harness uninstall <kind>` records that
   in `state.json`; only `amux harness install` or `pin` opts back in.
5. **No recent attempt failed.** A failed adoption (offline, download error) is
   retried after 6 hours; `amux harness ls` shows the error.

The version installed is the channel's, or the version of your own binary when
that is newer, so adoption never moves new sessions to an older release than
the one you were running. `amux harness adopt --dry-run` prints each decision
and its reason, as seen from your shell.

The daemon's own decisions are what count, and its environment can differ from
your shell's. Each harness's last decision (`adopt` or `skip`, with its reason
and time), an attempt still installing, and the last failure are stored in
`state.json`. `amux harness ls` shows them on an `adoption` line, and `amux
doctor` under each harness. The daemon also logs every decision, declines
included, once per run in `daemon.log` (`harness updates: not adopting claude:
… [binary: …; login: …]`), with the binary and login it found.

A login shell's `command -v` counts only when it prints an absolute path to an
executable. Startup-file output and alias or function definitions are ignored,
and resolution moves on to the usual install locations.

### Running sessions move to the new version

When a newer managed version becomes active (an automatic update, `amux harness
update`, `install` or `pin`), the daemon restarts every running session that is
on an older managed version onto the new binary, one session at a time, and
resumes the same conversation. It runs this pass every 20 seconds and right
after an update.

A session is restarted only at a **safe point**, when it is idle between turns:

- **PTY sessions** (Claude Code, Codex TUI): the harness's turn state (hooks,
  or for Codex the rollout's recent writes) is idle/ready, not running and not
  waiting on a permission prompt, and no question selector is open.
- **App Server sessions** (Codex structured control): the supervisor has no
  active turn (whoever started it), no prompt of amux's between `turn/start`
  and its completion, and no open approval.
- **Nothing was just delivered**: no prompt, interjection, steer or permission
  answer reached the session in the last 30 seconds (typed input may still be
  queued, and hook state lags a moment behind).
- The session has stayed idle for 30 seconds (two observations).

Right before stopping the old process, the daemon takes the session's
admission lock (the lock every prompt, start and restart of that session
holds) and checks all of this again. A prompt that arrives during the restart
waits for the new runtime; one that arrived just before is visible as a busy
turn or a recent delivery, and the restart waits for the next idle window. A
turn is never interrupted.

The relaunch is the agent-restart path (the same one `amux restart` and the
daemon's restart journal use): the restart intent is captured from the old
runtime and the conversation id is unchanged, so the harness resumes it —
`claude --resume <id>`, `codex resume <id>`, or the App Server's
`thread/resume` from the session's persisted thread identity. A session the
relaunch cannot resume is **not** restarted, and stays on its version until
you restart it:

- a Claude Code session with no saved transcript (it has not run a turn), or
  without a pinned conversation id;
- a Codex TUI session with no saved rollout;
- an App Server session whose thread has not run a turn (its identity is
  known-unrun, so a relaunch would start a new thread).

A relaunch without a conversation to resume would start a fresh one (and mark
the session "couldn't resume pinned conversation"), which is not something to
do silently.

**Failures fall back.** If the new version does not launch, the daemon
relaunches the session on the version it was running (same resume path) and
retries the update after 1 minute, then 5, 30 minutes, 2 and 6 hours. A new
version that exits within 20 seconds of starting is handled the same way. If
the old version cannot launch either, the daemon tries the active one again
rather than leave the session dead. GC keeps every version a process still
executes, and the previous version, so the fallback target is normally still
installed.

`amux status` (on stderr) and `amux doctor` list each waiting session, e.g.
`a1b2c3 restart pending for harness update (claude 2.1.311 → 2.1.325): waiting
for the agent to be idle (mid-turn or waiting on a permission prompt)`.

Only sessions launched from a managed install move. A session launched from
your PATH binary or an override keeps running it; adopting a managed install
does not restart such sessions (their next restart launches the managed copy).
A rollback (the active version older than a session's) restarts nothing.

### Opting out

- `amux config set harnesses.update off` (or `notify`): no adoption and no
  automatic installs. Already adopted installs stay; `amux harness uninstall
  <kind>` returns a harness to your PATH binary.
- `AMUX_NO_HARNESS_AUTO_UPDATE=1` in the daemon's environment (and when running
  `scripts/install.sh`): the same as `off`, whatever the config says. Restart
  the daemon after setting it.
- `amux config set harnesses.restart_sessions off`: updates still install, but
  running sessions keep their version until you restart them.
- `AMUX_<KIND>_BIN` / `harnesses.<kind>.bin`: launch exactly that binary; amux
  neither adopts nor restarts sessions for that harness.

## What happened before

Before this change, amux resolved `claude` / `codex` through PATH every time it
launched a session. A self-updating global install worked against that in two
ways.

- **Claude Code's native installer** keeps `~/.local/share/claude/versions/<v>`
  and moves the `~/.local/bin/claude` link to a new version in the background.
  Sessions already running kept their old binary, which was fine. But each pane
  restart, or each new session, could launch a different version without
  warning, and old versions could be pruned while amux sessions still used them.
- **Inside a protected pane** the updater cannot write the host install. All it
  could do was fail, or prompt.
- **Codex** showed an "update available" prompt in every TUI. Updating through
  npm or brew replaced the files under running sessions. The App Server and the
  `--remote` attach could then end up on different versions.

## Assumptions and limits

- Linux (glibc or musl) and macOS, on x86_64 and arm64. Other platforms keep
  using PATH.
- Hermes and any other harness can only use the override or PATH.
- Harness updates never require a daemon restart. amux's own updates are
  separate (see [versioning.md](versioning.md)) and use the same safe point.
