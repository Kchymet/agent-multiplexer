# amux client/server architecture

Splits amux into three roles connected by two protocols, so the UI is a thin
client that can drive a **local** multiplexer or any number of **remote** ones,
each orchestrating agents on its own machine.

```
┌────────┐   UI ⇄ Server protocol    ┌────────────────┐  Server ⇄ Harness proto  ┌──────────────┐
│   UI   │ ───── (muxproto) ───────▶ │  Multiplexer   │ ─── (harnessproto) ────▶ │ Agent Harness │
│ client │ ◀──── newline-JSON ────── │     Server     │ ◀──── newline-JSON ───── │  (PTY owner)  │
└────────┘   unix (local) / TCP      └────────────────┘   stdio / net.Pipe       └──────────────┘
  renders        (remote)              owns state +                                runs claude /
  vterms                               routes pane I/O                             editor / shell
```

## Roles

- **UI (client)** — renders the rail and the embedded agent panes. Owns no agent
  processes. It connects to one or more multiplexer servers (local socket and/or
  remote `host:port`), subscribes to their state, and opens **pane streams** for
  the agent it's viewing: server bytes feed a local vterm, keystrokes/resizes go
  back over the wire. Identical code talks to a local or remote server.

- **Multiplexer Server** — the backend. Owns the session model (`store`), derives
  the rail snapshot (`source`), and performs lifecycle actions (`wsops`):
  create/move/archive workgroups & agents. It does **not** run agent processes
  itself — it delegates that to a harness and multiplexes the resulting pane I/O
  to subscribed UI clients. One server per machine; remote servers are peers a UI
  can additionally connect to. `amux serve [--listen unix:PATH|tcp:ADDR]`.

- **Agent Harness** — owns the actual processes. Given a pane spec (argv, dir,
  env) it spawns the process in a PTY and streams its output; it accepts input,
  resize, and kill. This is the unit that could later run in a container, a jail,
  or a different host. `amux harness` speaks the protocol over stdio; the server
  spawns one (or embeds one via `net.Pipe`).

## Transport & framing

Both protocols are **newline-delimited JSON** over a byte stream. One JSON object
per line; pane payload bytes are base64 in the `data` field (terminal I/O for a
single viewer is low-throughput; a binary framing is a later optimization). Local
links use a unix socket / stdio / `net.Pipe`; remote links use TCP. Every
connection opens with a `hello`/`welcome` carrying a protocol `version` so peers
can refuse mismatches. (Remote auth/TLS is a documented follow-up; bind v1 TCP to
trusted networks.)

## Protocol 1 — UI ⇄ Multiplexer Server (`internal/muxproto`)

Client → Server (`ClientMsg.type`):
- `hello` `{version}` — open; server replies `welcome`.
- `subscribe` — start receiving `snapshot` frames (the rail state).
- `action` `{action,id,target,fields}` — lifecycle (open/delete/move/archive/
  new-repo-agent/new-workgroup/…); mirrors today's `core.Action`.
- `pane.open` `{paneId,agent,tab}` — start streaming a pane (tab 0 agent / 1
  editor / 2 terminal). The client mints `paneId`.
- `pane.input` `{paneId,data}` — keystrokes (base64).
- `pane.resize` `{paneId,cols,rows}`.
- `pane.close` `{paneId}`.

Server → Client (`ServerMsg.type`):
- `welcome` `{version,server}` — server identity/capabilities.
- `snapshot` `{sessions}` — the `[]core.Session` rail state (push on change).
- `result` `{ok,error}` — action ack.
- `pane.output` `{paneId,data}` — process output (base64).
- `pane.exit` `{paneId,error}` — the pane's process ended.

## Protocol 2 — Multiplexer Server ⇄ Agent Harness (`internal/harnessproto`)

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

## Rollout (non-breaking)

The new packages live alongside the existing daemon/TUI. The TUI gains a client
path used when `AMUX_SERVER` is set (or always, with an in-process server for the
local case); the legacy direct-spawn path remains the default until the client
path reaches parity. This lets the protocols land and be exercised without
breaking the working local app.
