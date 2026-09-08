# amux client/server architecture

Splits amux into three roles connected by two protocols, so the UI is a thin
client that can drive a **local** multiplexer or any number of **remote** ones,
each orchestrating agents on its own machine.

```
┌────────┐   UI ⇄ Server protocol    ┌────────────────┐  Server ⇄ Harness proto  ┌──────────────┐
│   UI   │ ───── (muxproto) ───────▶ │  Multiplexer   │ ─── (harnessproto) ────▶ │ Agent Harness │
│ client │ ◀──── newline-JSON ────── │     Server     │ ◀──── newline-JSON ───── │  (PTY owner)  │
└────────┘   TLS over unix / TCP     └────────────────┘   inherited net.Pipe      └──────────────┘
  renders        + bearer              authenticated relay                         runs claude /
  vterms                               + pane routing                              editor / shell
```

## Roles

- **UI (client)** — renders the rail and the embedded agent panes. Owns no agent
  processes. It connects to one or more multiplexer servers (local socket and/or
  remote `host:port`), subscribes to their state, and opens **pane streams** for
  the agent it's viewing: server bytes feed a local vterm, keystrokes/resizes go
  back over the wire. Identical code talks to a local or remote server.

- **Multiplexer Server** — a legacy compatibility translator. It authenticates
  to the singleton primary daemon for every snapshot, lifecycle action, and
  typed pane launch specification; it never opens the store or access authority.
  It delegates the resulting pane launch to an embedded harness and multiplexes
  pane I/O to authenticated UI clients. `amux serve [tls:HOST:PORT]`.

- **Agent Harness** — owns the actual processes. Given a pane spec (argv, dir,
  env) it spawns the process in a PTY and streams its output; it accepts input,
  resize, and kill. This is the unit that could later run in a container, a jail,
  or a different host. The legacy mux embeds one over a parent-owned `net.Pipe`.
  Raw `amux harness` stdio is disabled because stdio alone authenticates no peer.

## Transport & framing

Both protocols are **newline-delimited JSON** over a byte stream. One JSON object
per line; pane payload bytes are base64 in the `data` field. Client links use
TLS over either Unix or TCP; the embedded harness uses a parent-created
`net.Pipe`. Every client connection opens with `hello`/`welcome` carrying a
protocol `version`; all first-frame, version, and token failures receive the
same terminal `unauthorized` response.

### Client transport security

A mux link runs over **TLS** and requires a **nonempty bearer token**, wrapping
the raw `net.Conn` under the wire framing (message handling is unchanged):

- **TLS** — `amux serve tls:HOST:PORT` presents the cert/key from `$AMUX_TLS_CERT`
  / `$AMUX_TLS_KEY` (optional `$AMUX_TLS_CLIENT_CA` enables mutual TLS). A client
  dials `tls:HOST:PORT` and verifies the server against the system roots plus an
  optional private CA (`$AMUX_TLS_CA`), with an optional server-name override
  (`$AMUX_TLS_SERVERNAME`). The shared helpers live in `internal/wiretls`.
- **Token** — `$AMUX_MUX_TOKEN` must be nonempty and the client presents it in
  its first `hello` (constant-time compared). Missing/mismatched credentials,
  wrong versions, non-hello first frames, and repeated hello all close the
  connection without exposing state.

Plain TCP is refused, including loopback. The default Unix listener is also
wrapped in TLS and the client verifies it with `$AMUX_TLS_CA` plus, when needed,
`$AMUX_TLS_SERVERNAME`; the pathname is routing rather than server identity.
This prevents a substituted Unix endpoint from soliciting the bearer. Native
local UI continues to use the primary daemon directly; it does not silently
start or fall back to this compatibility service.

## Protocol 1 — UI ⇄ Multiplexer Server (`internal/muxproto`)

Client → Server (`ClientMsg.type`):
- `hello` `{version,token}` — mandatory first frame; the token is never blank.
- `subscribe` — start receiving `snapshot` frames (the rail state).
- `action` `{action,id,target,fields}` — lifecycle (open/delete/move/archive/
  new-repo-agent/new-workgroup/…); mirrors today's `core.Action`.
- `pane.open` `{paneId,agent,tab}` — start streaming a pane (tab 0 agent / 1
  editor / 2 terminal). The client mints `paneId`.
- `pane.input` `{paneId,data}` — keystrokes (base64).
- `pane.resize` `{paneId,cols,rows}`.
- `pane.close` `{paneId}`.

Server → Client (`ServerMsg.type`):
- `welcome` `{ok,version,server,error?}` — server identity/capabilities, or the
  generic terminal rejection `unauthorized` before close.
- `snapshot` `{sessions}` — the `[]core.Session` rail state (push on change).
- `result` `{ok,error}` — action ack.
- `pane.output` `{paneId,data}` — process output (base64).
- `pane.reset` `{paneId}` — the server fell too far behind to stream losslessly
  and is about to replay a fresh repaint; the client must clear its emulator for
  the pane before applying subsequent output (else stale cells ghost through).
- `pane.exit` `{paneId,error}` — the pane's process ended.

Pane output is streamed **losslessly**: a terminal byte stream is stateful, so
the server coalesces per-pane output rather than dropping bytes from the middle
(which would corrupt the client's emulator). A client that falls catastrophically
far behind (past a 4 MiB per-pane cap) is trimmed to the most recent 256 KiB tail
preceded by `pane.reset`, bounding memory without silent corruption. Discrete
frames (snapshots, results) remain droppable — each is a full state.

## Protocol 2 — Multiplexer Server ⇄ Agent Harness (`harnessproto`)

Server → Harness (`MuxMsg.type`):
- `hello` `{version}`.
- `spawn` `{paneId,dir,env,argv,cols,rows}` — run a process in a PTY.
- `input` `{paneId,data}`.
- `resize` `{paneId,cols,rows}`.
- `kill` `{paneId}`.

Harness → Server (`HarnessMsg.type`):
- `ready` `{version}` — harness up.
- `output` `{paneId,data}`.
- `exit` `{paneId,error}` — process ended (clean or with error).

## How pane streaming replaces local PTYs

Today the native TUI spawns the agent/editor/shell PTY itself and renders it. In
the split, the **server** (via the harness) owns the PTY; the UI's vterm is fed
by `pane.output` frames and forwards keys via `pane.input`. The vterm already
emulates a screen from a byte stream, so the only change on the UI side is the
byte source: a server stream instead of a local `*os.File` PTY.

## Compatibility boundary

The native TUI retains its direct authenticated primary-daemon path. It does not
auto-start or fall back to the legacy mux. Operators who explicitly run the
compatibility relay must configure its TLS identity and nonempty bearer; the
relay then reauthenticates to the primary daemon for each snapshot, action, and
typed launch specification.
