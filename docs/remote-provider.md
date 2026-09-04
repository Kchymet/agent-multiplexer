# amux as a remote compute provider

Status: Implemented. `amux provide` (package `internal/provider`) drives the v2
protocol end to end.

`amux provide install` runs it as a user service (systemd on Linux/WSL2, launchd
on macOS) from a config file, and `amux doctor` reports it — see "The two-command
laptop setup" below.

Landed: the TLS + bearer-token seam at the wire boundary (`internal/wiretls`,
shared with the mux server — see `client-server.md`), the harnessproto v2 message
types and codec (`register`/`registered`/`ping`/`pong`/`reset`, per-pane `seq`),
version negotiation and a constant-time token check, and — in `internal/provider`
— the dial-out FSM, jittered exponential reconnect/backoff, per-pane replay
buffers (4 MiB cap / 256 KiB keep-tail + `reset`), pane survival across
disconnects within the grace window, `PaneOffer`/`AdoptPane` resume with
`afterSeq` replay, and the ping/pong heartbeat. See the `amux provide` command
under "Provider mode UX" below.

amux's harness already runs pane execution behind a protocol
(the published `harnessproto` module — see `docs/client-server.md`): an orchestrating side
sends `spawn/input/resize/kill`, the executing side streams `output/exit`
back, as line-framed JSON over any byte stream. This document specifies
**provider mode**: the amux daemon dials out to a **remote orchestrator**,
registers itself, and serves that same protocol over the connection — turning
any machine running amux into a compute node the orchestrator can schedule
agent processes onto.

The orchestrator is any service that speaks this contract. amux contains no
knowledge of, or code for, any particular orchestrator.

```
   provider machine (amux)                      remote orchestrator
┌───────────────────────────┐   TCP + TLS    ┌──────────────────────┐
│ amux provider mode        │───── dial ────▶│ TLS listener         │
│  register {token, caps} ─▶│                │ verifies token       │
│                          ◀│── registered ──│                      │
│  then harnessproto v2:   ◀│── spawn/input──│ schedules work,      │
│  PTY owner, output buffer │─── output ────▶│ consumes I/O         │
│  ping ───────────────────▶│◀─── pong ──────│                      │
└───────────────────────────┘                └──────────────────────┘
```

## Trust model — read this first

Registering with an orchestrator hands it **arbitrary code execution on this
machine, as your user** (that is the feature — the same trust shape as a
self-hosted CI runner). Only register with orchestrators you control or
trust. Mitigations on the provider side:

- Run the provider as a dedicated, minimally-privileged user.
- amux's bubblewrap sandboxing travels inside the spawned argv; advertise
  `bwrap` in capabilities and prefer orchestrators that use it.
- Labels let you constrain what the orchestrator schedules here by
  convention; they are advisory, not enforcement.
- TLS is mandatory in provider mode; there is no plaintext option. The token
  is a bearer credential — protect it like an SSH key (file mode 0600,
  `--token-file`, never argv).

Conversely, the orchestrator gets nothing else: providers only dial out, hold
no inbound listener, and expose no filesystem/API beyond the panes it spawns.

## Connection model

- One long-lived **TCP+TLS** connection, initiated by the provider (works
  behind NAT; no inbound port). All traffic — registration, heartbeats, pane
  I/O — multiplexes over it using the existing `internal/wire` line-JSON
  framing (one JSON object per line, `[]byte` as base64).
- TLS: standard hostname + chain verification against the system roots, with
  an optional CA file for private CAs. Authentication is a bearer token
  inside the TLS channel, issued by the orchestrator's operator.
- Reconnect: jittered exponential backoff (1s doubling to a 30s cap),
  forever — except terminal registration errors (`bad-token`, `revoked`,
  `unsupported-version`), which exit with a message instead of retrying.

## Protocol (harnessproto v2)

v2 is an additive extension of the v1 protocol in `docs/client-server.md`.
Message direction follows harnessproto: provider sends `HarnessMsg`, receives
`MuxMsg`. v1 (in-process/stdio harness, `hello`/`ready`, no auth, no seq) is
unchanged and still spoken by `amux harness`.

### Provider → orchestrator

- `register` — first message on every connection:
  `{versions:[1,2], token, name, labels:{...}, capabilities:{maxPanes, bwrap,
  os, arch, features:[]}, panes:[{paneId, outSeq, running}]}`.
  `panes` offers panes still running from a previous connection (resume);
  empty on cold start.
- `output` `{paneId, data, seq}` — pane bytes; `seq` is per-pane, monotonic
  from 1, counted in frames.
- `exit` `{paneId, error?, seq}` — process ended; last frame of the pane.
- `reset` `{paneId, seq}` — replay buffer overflowed; frames before `seq` are
  gone. Consumers rendering a terminal must clear their emulator before
  applying subsequent output.
- `ping` `{t}` — heartbeat at the cadence the orchestrator sets.

### Orchestrator → provider

- `registered` `{ok, error?, version, providerId, heartbeatSeconds,
  graceSeconds, adopt:[{paneId, afterSeq}], kill:[paneId,...]}` — accepts or
  terminally rejects the registration; negotiates the version (highest
  common; no overlap ⇒ `unsupported-version`); resolves the resume offer
  (every offered pane is adopted or killed — omission means kill). For
  adopted panes the provider retransmits output frames `> afterSeq`.
- `spawn` `{paneId, dir, env, argv, cols, rows}` / `input` / `resize` /
  `kill` — exactly v1. The environment split holds: the provider supplies the
  local execution environment, the orchestrator supplies workload-specific
  vars (see `internal/harness/harness.go`).
- `pong` `{t}`.

### Liveness and disconnect semantics

- Heartbeat every `heartbeatSeconds` (default 15); either side treats 4
  missed intervals as a dead connection.
- **A dropped connection does not kill panes.** Processes keep running for
  `graceSeconds` (default 60) while output accumulates in per-pane replay
  buffers (bounded, default 4 MiB per pane). Reconnect within grace →
  register offers the panes, the orchestrator adopts, and output replays
  losslessly from `afterSeq`. If the gap exceeded the buffer, the provider
  trims to the most recent 256 KiB tail and sends `reset` first — bounded
  memory, bounded resync, never silent terminal corruption.
- Grace expiry, or the orchestrator listing a pane under `kill`: the pane is
  terminated and its buffer discarded. A provider process restart loses all
  panes (PTYs are children); the next `register` simply offers none.
- Operator stop (SIGINT/SIGTERM): the current implementation terminates panes
  and closes the connection immediately, rather than draining (letting panes
  exit and report first). Graceful drain-on-stop is a future refinement.

### Spawn conventions

`dir` must be a path valid on this machine; orchestrators either target
providers whose labels promise the needed layout or send argv that prepares
its own directory. `paneId`s are minted by the orchestrator and stable across
reconnect+adopt. First-class workspace provisioning (a `prepare` message) is
a possible future extension, not part of v2.

## Provider mode UX

### The two-command laptop setup

Registering a machine should not mean a terminal that has to stay open. Install
the provider as a **user service** — a systemd user unit on Linux/WSL2, a launchd
agent on macOS — and it starts at login, restarts on exit, and survives a reboot:

```sh
# 1. put the bearer token somewhere only you can read
install -m 600 /dev/null ~/.config/amux/provider.token
printf '%s' "$TOKEN" > ~/.config/amux/provider.token

# 2. write the config and install the service
amux provide install --orchestrator orch.example.com:7443 \
                     --token-file ~/.config/amux/provider.token \
                     --name laptop --label zone=home

amux doctor          # Provider section: config, token, service, last heartbeat
```

`amux provide install` writes `~/.config/amux/provider.toml` and the service
unit; `amux provide uninstall` stops and removes the service (the config file and
token are kept, so reinstalling is one command). `amux provide` with no arguments
reads the config file — that is exactly what the service runs.

**The token is never in the config file or in argv.** The config names the *path*
to a mode-0600 token file, so rotating the credential is a single write with no
reinstall, and a config that leaks is not a credential that leaks. Install
tightens the token file's mode if it is looser than 0600, and doctor keeps
checking it.

| | |
|---|---|
| Config file | `~/.config/amux/provider.toml` (honors `$XDG_CONFIG_HOME`) |
| Linux/WSL2 service | `~/.config/systemd/user/amux-provide.service` |
| macOS service | `~/Library/LaunchAgents/com.kchymet.amux.provide.plist` |
| Status file | `~/.local/state/amux/provider-status.json` |

`install` takes the address positionally too, with flags on either side of it,
exactly as running the provider does.

Re-running `install` merges over the existing config, so `amux provide install
--name box` changes one setting and keeps the rest, and re-running it after
`make install` re-points the service at the new binary and restarts it.
`--dry-run` prints the config and the unit without writing either.

**Linux and WSL2: enable lingering.** A systemd *user* service only runs while
you have a session, so without lingering the provider dies when your last
terminal closes — the exact failure the service was meant to prevent:

```sh
loginctl enable-linger $USER
```

`amux provide install` and `amux doctor` both say so when it is off. WSL2 also
needs systemd itself turned on — put

```ini
[boot]
systemd=true
```

in `/etc/wsl.conf` and run `wsl --shutdown` from Windows. Install says so too if
the user bus is not reachable.

### The config file

```toml
# ~/.config/amux/provider.toml — written by `amux provide install`
orchestrator = "orch.example.com:7443"
token-file = "/home/you/.config/amux/provider.token"
name = "laptop"
max-panes = 8
publish-sessions = true
features = ["bigdisk", "cuda"]

[labels]
zone = "home"
```

The keys are the `amux provide` flag names verbatim, so the file reads like the
command line it replaces. amux reads the subset of TOML it writes: a key it does
not recognize is an error you see, never a setting silently dropped.

### Running it by hand

```
amux provide orch.example.com:7443 \
             --token-file ~/.config/amux/provider.token \
             --label zone=home --label gpu=none \
             --feature cuda --feature bigdisk \
             [--ca /path/to/private-ca.pem] [--name mybox] \
             [--max-panes 8] [--server-name mybox.internal]
```

The orchestrator address is the positional argument (or `--orchestrator`); a
`tls:` scheme prefix is accepted and stripped (provider mode is always TLS).
Flags may come before or after the address — `amux provide host:7443 --ca ca.pem`
and `amux provide --ca ca.pem host:7443` are the same command. Giving the address
twice, or leaving a stray word on the line, is an error rather than a silent
choice — a flag that goes unread surfaces much later as something else entirely
(an unread `--ca` looks exactly like a bad certificate).

Logs report the FSM plainly: dialing, registered (with negotiated version and
providerId), disconnect/grace, backoff, and terminal errors.

Configuration resolves from flags first, then these env vars (matching amux's
`AMUX_*` convention), then the config file:

| Setting | Flag | Env var |
|---|---|---|
| Bearer token | `--token-file` (path; never argv) | `AMUX_PROVIDER_TOKEN` |
| Display name | `--name` | `AMUX_PROVIDER_NAME` (default: hostname) |
| Scheduling labels | `--label k=v` (repeatable) | `AMUX_PROVIDER_LABELS` (comma-separated `k=v`) |
| Feature capabilities | `--feature s` (repeatable) | `AMUX_PROVIDER_FEATURES` (comma-separated) |
| Max panes capability | `--max-panes` | `AMUX_PROVIDER_MAX_PANES` |
| Private CA | `--ca` | `AMUX_TLS_CA` |
| TLS server name | `--server-name` | `AMUX_TLS_SERVERNAME` |

Flags and env vars merge for labels and features (flags win on conflict), and the
config file supplies whatever neither set — so the service's bare `amux provide`
is fully configured by the file, while an ad-hoc run can still override any one
setting.

### Status file and doctor

The provider loop writes `~/.local/state/amux/provider-status.json` on every
state change: `dialing` → `registered` → `disconnected`/`rejected`/`stopped`,
plus the negotiated `providerId`, the last successful registration, the last
heartbeat, the live pane count, and the last error. A provider that is running
but has never been accepted looks identical to a working one from the outside;
this file is the difference, and `amux doctor`'s **Provider** section reads it:

```
Provider
  ✓ config    /home/you/.config/amux/provider.toml
              orchestrator orch.example.com:7443 · name laptop · sessions
  ✓ token     /home/you/.config/amux/provider.token (mode 0600)
  ✓ service   systemd (user) · active
              /home/you/.config/systemd/user/amux-provide.service · enabled at login
  ✓ status    registered (3s ago)
              providerId prov-42 · registered 2m14s ago · heartbeat 3s ago · 2 panes
```

Nothing here can fail the health check — provider mode is opt-in, so "not
configured" is a normal, healthy state. Doctor does contradict the one lie the
file can tell: a `registered` record whose process is gone (a SIGKILL, a reboot)
is reported as stale rather than repeated.
Feature strings are opaque: amux never interprets or hardcodes them — the
orchestrator matches on them by convention. `bwrap`, `os`, and `arch`
capabilities are detected automatically (`bwrap` is probed on `$PATH`).

## Failure behavior summary

| Event | Provider behavior |
|---|---|
| Bad/revoked token, version mismatch | exit with message; no retry |
| TLS verification failure | log + backoff (cert may be rotating) |
| Orchestrator restart / network flap | backoff-reconnect; panes survive within grace; lossless replay or trim+`reset` |
| Grace exceeded | kill orchestrator-owned panes, discard buffers |
| Buffer overflow (slow link, chatty pane) | trim to tail + `reset`; other panes unaffected |
| Malformed frame | close connection (line-JSON has no resync); reconnect recovers |
| Provider process crashes | the user service restarts it after `RestartSec` (5s) |
| Repeated terminal rejection | systemd's start limit gives up after 10 tries in 5 min; the service shows as failed and doctor reports it |
