# amux as a remote compute provider

Status: Protocol v2 primitives implemented; provider mode (the `amux provide`
command that drives them) not yet wired.

Landed: the TLS + bearer-token seam at the wire boundary (`internal/wiretls`,
shared with the mux server — see `client-server.md`), the harnessproto v2 message
types and codec (`register`/`registered`/`ping`/`pong`/`reset`, per-pane `seq`),
version negotiation and a constant-time token check. Still to build (provider
mode): the dial-out FSM, reconnect/backoff, per-pane replay buffers, and the
`amux provide` CLI described under "Provider mode UX" below.

amux's harness already runs pane execution behind a protocol
(`internal/harnessproto` — see `docs/client-server.md`): an orchestrating side
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
- Operator stop drains: no new spawns, panes exit and report, then the
  connection closes.

### Spawn conventions

`dir` must be a path valid on this machine; orchestrators either target
providers whose labels promise the needed layout or send argv that prepares
its own directory. `paneId`s are minted by the orchestrator and stable across
reconnect+adopt. First-class workspace provisioning (a `prepare` message) is
a possible future extension, not part of v2.

## Provider mode UX (planned shape)

```
amux provide --orchestrator orch.example.com:7443 \
             --token-file ~/.config/amux/provider.token \
             --label zone=home --label gpu=none \
             [--ca /path/to/private-ca.pem] [--name mybox]
```

Config may also live in amux's config file (orchestrator address, token path,
labels, display name). `--name` defaults to the hostname. Logs report the
FSM plainly: dialing, registered (with negotiated version and providerId),
degraded, backoff, terminal errors.

## Failure behavior summary

| Event | Provider behavior |
|---|---|
| Bad/revoked token, version mismatch | exit with message; no retry |
| TLS verification failure | log + backoff (cert may be rotating) |
| Orchestrator restart / network flap | backoff-reconnect; panes survive within grace; lossless replay or trim+`reset` |
| Grace exceeded | kill orchestrator-owned panes, discard buffers |
| Buffer overflow (slow link, chatty pane) | trim to tail + `reset`; other panes unaffected |
| Malformed frame | close connection (line-JSON has no resync); reconnect recovers |
