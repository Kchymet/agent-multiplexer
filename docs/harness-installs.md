# amux-managed harness installs

amux can install its own standalone copies of Claude Code and Codex, separate
from anything you installed globally, and manage their updates. This is opt-in:
until you run `amux harness install`, amux launches whatever `claude` / `codex`
it finds on your PATH, as before.

```sh
amux harness install claude        # Claude Code, "stable" channel
amux harness install codex         # Codex, latest release
amux harness ls                    # what each harness launches, installed versions
amux harness update                # update every managed harness
amux harness pin claude 2.1.311    # activate and freeze a version
amux harness unpin claude
amux harness rollback codex        # re-activate the previous version
amux harness check                 # look for a newer release without installing
amux harness gc                    # delete versions nothing uses
amux harness uninstall codex       # go back to the Codex on your PATH
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
- **Running sessions keep their version.** At launch amux resolves `current` to
  the concrete version directory, so a session's argv names
  `…/claude/2.1.311/bin/claude`. Moving `current` later only changes what *new*
  launches (and restarts) get. A pane restart picks up the new version.
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

The daemon can check for updates on a schedule. It is off by default:

```sh
amux config set harnesses.update notify    # record + log available updates
amux config set harnesses.update auto      # install + activate them (not pinned ones)
amux config set harnesses.check_interval 12h   # default 24h, minimum 1h
amux config set harnesses.claude.channel latest  # claude: stable (default) | latest
```

- The daemon re-reads these settings every 15 minutes. You don't need to
  restart it.
- A check runs only when the last one is older than the interval. The time of
  the last check is stored in `state.json`, so restarting the daemon doesn't
  trigger a new check.
- Only harnesses with a managed install are checked. amux never installs a
  harness you have not opted into.
- **notify** records the newest version. `amux harness ls` and `amux doctor`
  then show `update X available`, and the daemon log has a line.
- **auto** also installs and activates the new version, then runs GC. It
  applies the same rules as a manual update: pinned harnesses are skipped, and
  running sessions keep their version.

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
- amux's own install and updates are unchanged (`make install`, then
  `amux daemon restart`). Harness updates never require a daemon restart.
