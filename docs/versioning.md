# Versioning and compatibility

`amux version` reports the current CLI build and, without starting anything,
queries the connected local daemon for its build and database schema. `amux
doctor` shows the same information as health checks and exits non-zero for a
known incompatibility.

There are three independent numbers:

- The product version identifies a build. Different CLI and daemon product
  versions are allowed when their protocol remains compatible.
- The daemon protocol identifies the local CLI↔daemon API. Additive queries and
  fields retain the protocol number; a breaking wire or behavior change bumps
  it. Doctor currently accepts an exact match.
- SQLite `PRAGMA user_version` identifies the database schema. Schema zero is
  the legacy unmarked database; the current schema is three. Each daemon reports
  the schema range it can migrate/open and refuses a database outside that range
  before applying migrations.

This keeps compatibility decisions attached to the contracts that matter. A
patch release does not become incompatible merely because its label differs,
while a protocol or schema break cannot hide behind an unchanged executable
name.

## Product versions

The product version comes from git (`scripts/version.sh`; `make version` prints
it):

| Build | Version | Example |
|---|---|---|
| Release (CI, `make … RELEASE=1`) | `0.1.<first-parent commit count>` | `0.1.342` |
| Checkout (`make build`/`install`) | `0.1.<count>-dev+<sha12>[.dirty]` | `0.1.342-dev+2922c179ad56.dirty` |
| Plain `go build` | `0.0.0-dev[+<sha12>[.dirty]]` | `0.0.0-dev+2922c179ad56` |

`master` is squash-merged and linear, so the commit count increases with every
push and every release is newer than the one before. The `-dev` prerelease sorts
a checkout build below the release of the same count and marks it as a
development build. `amux version` prints it; `amux version --short` prints only
the version (and never contacts the daemon); `amux version --manifest` prints
the build's version, commit, daemon protocol and schema range as JSON.

## Releases

`.github/workflows/release.yml` runs after CI passes on every push to `master`.
It builds `make release-assets RELEASE=1` — one binary per platform
(`amux-darwin-amd64`, `amux-darwin-arm64`, `amux-linux-amd64`,
`amux-linux-arm64`), each with the same notices-check and license-verify
provenance as `make build` — plus `amux.sh`, `amux-release.json` (the manifest
above, printed by the built binary) and `SHA256SUMS`, and publishes them as
release `v0.1.<count>` with the built-in `GITHUB_TOKEN`. A delayed run for an
older commit never moves "latest" backwards.

## Upgrading

Install the latest release with `scripts/install.sh` (see the README), or from
an existing install:

    amux update --check          # report installed vs latest; changes nothing
    amux update                  # install the latest release
    amux update --force          # also replace a checkout build / reinstall
    amux update --restart-daemon # restart the daemon even while agents run

`amux update` reads `/releases/latest/download/amux-release.json`, downloads
this platform's binary from that exact tag, verifies it against the release's
`SHA256SUMS`, runs it (`version --short`) to confirm it executes here, and then
renames it over the running executable (symlinks resolved; same directory, so
the replace is atomic and processes running the old binary keep its inode). It
refreshes `~/.config/amux/amux.sh` from the same release. Update state lives in
`~/.local/state/amux/update/` and is serialized by a lock, so the CLI and the
daemon never update at once.

### The daemon and running agents

The daemon hosts every agent's process, and stopping it stops them. The
restart journal relaunches each agent with its conversation afterwards
(`claude --resume`, `codex resume`, or the App Server's `thread/resume` from
the session's thread identity), so a restart while an agent is idle between
turns loses nothing; a restart mid-turn would interrupt the turn. An update
therefore restarts the daemon only at a **safe point**: every hosted agent is
idle between turns.

- No daemon running: the binary is replaced; the next start uses it.
- Daemon idle (no agents): `amux update` replaces the binary and restarts it.
- Agents running, same protocol and schema: the binary on disk is replaced at
  once (new CLI processes use it and still speak the running daemon's
  protocol); the daemon keeps its old code and restarts itself into the new
  binary at the safe point.
- Agents running, protocol or schema changes: swapping the binary would leave
  new CLIs unable to talk to the old daemon, so the release is *staged*
  (downloaded and verified, binary untouched). It is installed at the safe
  point, by the next `amux daemon restart`/`start`, or now with
  `amux update --restart-daemon`.
- A release whose schema range does not include this database's schema is
  refused; amux never lowers `user_version`.

The safe point is the one harness updates use (see
[harness-installs.md](harness-installs.md#running-sessions-move-to-the-new-version)),
observed on two consecutive polls (5 minutes apart) for every hosted runtime:

- each agent is idle between turns: not mid-turn, not waiting on a permission
  prompt, approval or question, no prompt of amux's starting a turn on an App
  Server, and no prompt, steer or permission answer delivered in the last 30
  seconds;
- each agent has a saved conversation the relaunch resumes. An agent that has
  not run a turn yet (no Claude transcript, no Codex rollout, an App Server
  thread known not to have run) would come back as a new conversation, so it
  holds the restart back like a busy agent;
- no editor or terminal pane is open: a shell or editor process cannot be
  resumed, so one blocks the restart.

Right before restarting, the daemon takes every hosted agent's admission lock
(the lock each prompt, start and restart of that agent holds), checks all of
this again, and only then commits. From that moment a prompt to any agent is
refused with "the daemon is restarting to load an amux update" instead of
being typed into a process about to stop; the agents come back within seconds.

`amux version`, `amux doctor` (Versions → update) and `amux ls` (on stderr)
report a pending update and why it is waiting, naming the agents that hold it
back (for example `waiting for 1 running agent(s) to be idle between turns
(a1b2c3: mid-turn or waiting on a permission prompt)`).

### Automatic updates

On by default. A release daemon checks at most every 6 hours (measured from the
persisted last check, so restarts do not re-query) and installs new releases
following the rules above. It never replaces a development build: neither a
dev daemon nor a checkout build installed at the binary's path. To opt out, run
`amux config set update.auto off` (read at each check; no restart needed) or set
`AMUX_NO_AUTO_UPDATE=1` in the environment the daemon starts from, then restart
it. A checkout build switches to releases (and from then on to automatic
updates) with `amux update --force`. `AMUX_UPDATE_REPO_URL` points updates at a
fork or mirror with the same release layout.

SHA256SUMS protects against truncated or corrupted downloads; authenticity
rests on HTTPS to GitHub and the repository's release permissions.

## Upgrade behavior

Build with Go 1.26.11 or a newer patched release. The Go standard library is part
of the binary, so updating a Go installation alone does not fix an old amux
executable. Rebuild, reinstall and restart the daemon; reopen dashboards and
restart any provider service. `go version -m /path/to/amux` reports the build's
Go version and, for a Git checkout, revision. `make vuln` checks reachable
vulnerabilities in both modules using the root module's selected toolchain.

Schema three promotes legacy hidden repository workgroups into normal visible
coordinator workgroups. Writable store opening preserves root/member IDs, member
sandboxes, branches, conversation IDs and archive state, adds a dedicated
`coordinator/` directory, and initializes explicit repository grants from the
legacy root's stored repo scope. Repository homes remain management context.
The migration does not move member files or launch runtimes. It is repeatable,
and `user_version` advances to three only when the promotion transaction commits;
a failed promotion from schema two leaves its marker at two so it can be retried.
Read-only store opening never migrates and requires the current schema.

For this upgrade, install the schema-three binary and restart the daemon before
resuming management operations; then reconnect dashboards and restart provider
services. Replacing only the CLI leaves the already-running daemon on its old
code. A schema-two daemon cannot open a database upgraded to schema three and
must be upgraded, not restarted with the older executable. Do not lower
`user_version` to make an older binary accept the promoted data.

The version query is additive. A daemon from before version reporting responds
with an unknown-query error; the CLI reports its version and database schema as
unknown and suggests `amux daemon restart`. It does not claim incompatibility
without evidence. Once both sides report contracts, doctor fails loudly on a
protocol mismatch or on a database outside the daemon's advertised schema
range.

The first schema marker cannot make already-installed, pre-versioning binaries
honor it retroactively. After upgrading across that boundary, restart the daemon
before using the database. All version-aware binaries refuse future schemas,
which prevents an older daemon from silently operating on a layout it does not
understand.

If the daemon protocol later needs rolling compatibility rather than exact
matching, evolve the single protocol number into advertised supported ranges (or
a negotiated version list) while keeping this query shape additive. Database
migrations should remain forward-only, idempotent, and set `user_version` only
after the corresponding migration succeeds.
