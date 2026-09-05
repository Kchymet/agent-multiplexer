# Codex App Server supervision (AGE-181)

Status: **opt-in, experimental.** Default Codex control stays the PTY path
(keystroke steering + rollout tailing). This document describes the structured
control mode amux offers when it supervises a Codex App Server itself. It reflects
the ROOT real-binary audit (Codex 0.153.4): the transport, protocol shapes, and
launch integration below are the corrected forms; the remaining host-validation
items are called out at the end.

## What it is

For a Codex session in **structured** control mode (`controlMode` is internal
routing metadata, docs/remote-provider-sessions.md §2.2 — *not* a user-facing
"PTY vs structured" choice), amux runs one background `codex app-server` **per
session** and is itself a client of it. The App Server's own listener accepts
multiple clients directly, so the **native Codex TUI in the amux pane** and the
**authenticated web bridge** are both clients of the *same server/thread* — amux
does **not** implement a multiplexer or facade:

- the web reaches the session through the existing authenticated provider contract
  (`sessions` + `runtime-events` + `session-action`); it never dials the App
  Server directly;
- the native CLI attaches **locally**, inside the sandbox, via
  `codex --remote <endpoint> resume <thread-id>`.

Protocol: `initialize` → `thread/start`|`thread/resume` → `turn/start`|`turn/steer`|
`turn/interrupt`, with server-initiated `.../requestApproval` and
`tool/requestUserInput` requests. amux's client and a native `--remote` peer speak
the identical wire, so they are interchangeable.

## Transport: WebSocket, never raw JSONL

The App Server listener speaks **JSON-RPC 2.0 over WebSocket (HTTP Upgrade)** — a
raw newline-delimited JSONL connection is closed immediately (ROOT audit). amux
dials it with a real WebSocket client (`internal/codexapp/wsconn.go`, built on
`golang.org/x/net/websocket`) and frames each JSON-RPC object as one text message.

Endpoints (configurable via `Config.Endpoint` / the server's `--listen`):

| endpoint | use |
| --- | --- |
| `unix://<per-session socket>` | **default** — local optimization that keeps the endpoint inside the session's private sandbox scope |
| `ws://127.0.0.1:<port>` | loopback, colocated clients (docs example) |
| `wss://host:port` | cross-machine, **authenticated TLS** — verification is never downgraded |

A non-loopback `ws://` (no TLS, no auth) endpoint is refused before dialing; an
unauthenticated non-loopback listener is never opened. Cross-machine ⇒ WSS. The
AGE-179 harness pilot keeps its stdio transport (not converted gratuitously).

## Protocol shapes (corrected against 0.153.4)

- `thread/start` policy enums are **hyphenated**: `on-request` / `workspace-write`
  (camelCase is rejected `-32600`).
- an approval is answered with an **object** `{"decision":"accept"|"decline"}`, not
  a bare enum string.
- `tool/requestUserInput` answers are a **map keyed by question id** — and amux
  does **not** auto-answer it: with other clients on the same server, answering
  empty would preempt a client (the native TUI) that can actually collect input, so
  the request is surfaced (`notice` + `raw`) and left open.

## Lifetime is independent of any UI pane

**The App Server's lifetime is owned by amux, bound to the daemon's context — never
a pane or a client connection.**

- The child runs in its own process group; a signal aimed at the foreground pane
  never reaches it.
- Closing the native TUI, or a client disconnecting, does **not** stop the server
  or interrupt an in-flight turn. **No process is killed by client disconnect.**
- `Supervisor.Close()` (archive/delete/leave-structured) *and* **daemon-context
  cancellation** both tear the server down (a watcher goroutine closes on
  `ctx.Done()`), so nothing is orphaned.

## Launch under the amux sandbox

The server is **not** a bare `exec`. The daemon resolves it through the same
sandbox wrapper as the agent's own pane (`panespec.AppServerCommand` →
`bwrap … -- codex app-server --listen <endpoint>`) so it inherits the session's
mount/config/identity scope and auth binds; cwd alone does not enforce that. The
supervisor receives that wrapped argv (`Manager.Ensure(..., wrappedArgv)`). Opening
the agent pane of a structured session launches `codex --remote <endpoint> resume
<thread-id>` (`panespec.AttachCommand`) — the native TUI attaches to the supervised
thread rather than starting a standalone Codex.

Per-session creation is **serialized** (a per-session lock taken before spawn), so
two callers never race to start competing servers on the same socket.

## Socket / thread identity persistence

amux persists, per structured session, `{endpoint, thread id, control mode}` as a
JSON sidecar under `<state>/codexapp/<session>.json`. It is amux-internal — never
on the wire (the endpoint lives in the private scope).

**Daemon-restart semantics.** On restart the daemon re-supervises the session:
fresh `codex app-server`, dial, then `thread/resume <thread-id>`. If the pinned
thread never ran a turn, `thread/resume` returns **"no rollout found"** (ROOT
probe); the supervisor then falls back to `thread/start` and adopts the new id
rather than failing the launch (the empty thread had no history to lose). The live
event seq space restarts from 1 per supervisor lifetime, which a consumer dedups by
seq like any tailer resync.

## Steering and events

- **Steer verbs** route to JSON-RPC (`turn/start`/`turn/steer`/`turn/interrupt` and
  an approval response) — not keystrokes. `prompt` runs the turn asynchronously
  (`accepted`); the shorter verbs answer with the supervisor's own error.
- **Any-origin turn tracking.** `turn_start`/`turn_end` are emitted from the
  *observed* `turn/started`/`turn/completed` notifications (filtered to the pinned
  thread), and the active turn id is captured from them — so a turn started in the
  **native TUI** is bracketed on the event stream and is steerable/cancelable from
  the web, not only turns amux's own `prompt` began.
- **Permissions are natively correlated.** An approval's JSON-RPC id is the
  `request_id` a `permission` verb echoes back; amux rejects a **stale** or
  **duplicate** reply and never guesses on an unparseable decision. It does **not**
  declare a request resolved just because the write succeeded — the App Server's own
  item/turn notification is the resolution signal, so every client converges on the
  same truth; a `turn/completed` clears any still-open approval
  (`permission_resolved{decision:"cleared"}`).
- **Events** are the App Server stream normalized to the shared runtime-event
  vocabulary (§4) — the same events the rollout tailer produces, one story
  regardless of control mode.

### Event transport (durable record, not a socket)

The provider that publishes `runtime-events` runs **out-of-process** (`amux
provide` tails on-disk records over the daemon socket; it cannot read the daemon's
in-memory hub). So a structured session's events reach a remote consumer through a
**daemon-owned durable record**: the supervisor appends each normalized event as a
JSON line to `<data>/codexapp/<id>.events.jsonl`, and `runtimeevents.sourcesFor`
reads a `Structured` record with the *identity* mapper — reusing the whole tailer
(seq, `afterSeq` resume, rotation) unchanged. Each supervisor lifetime truncates
the log; the tailer resyncs on the shrink and a consumer dedups by ordinal.

> If the pinned `codex app-server` turns out to write its own rollout jsonl (like
> `codex exec`), transcript events could come from the existing Codex rollout mapper
> and the supervisor would add only correlated approvals. That needs the host binary
> to confirm and is not assumed here.

## Opt-in and fallback

Default is the untouched PTY/exec path; the two are never wired at once for a
session. The selector is `AMUX_CODEX_CONTROL=app-server` (default off), read by the
daemon at launch through a single gate — `Daemon.structuredControl` — so the
default path is provably unaffected (`TestStructuredControlGate`).

## Validation posture — read before claiming it works

There is **no codex binary in the amux CI sandbox, and this work installs none.**

- The protocol/transport is covered by in-memory contract tests against a fake App
  Server (handshake start/resume incl. the empty-thread `thread/start` fallback,
  observed turn bracketing from any origin, interject, cancel, approval round-trip
  with the `{decision}` object + stale/duplicate rejection + no-speculative-resolve,
  user-input not auto-answered, disconnect-mid-turn, unknown request), plus a real
  WebSocket handshake + round-trip over unix and loopback TCP and the non-loopback
  refusal (`wsconn_test.go`).
- The daemon integration is covered without a binary: structured verb routing and
  refusals, the opt-in gate, the sandbox-wrapped launch resolution, the durable
  event record's identity mapping + permission replay, and the control-mode stamp.
  Legacy PTY steering tests pass unchanged.
- An **opt-in, self-skipping** smoke test (`AMUX_CODEX_APP_SERVER_SMOKE=1`,
  `TestSmokeRealAppServer`) drives a real `codex app-server` over a Unix WebSocket
  when a pinned binary is present, proving the handshake, a bracketed turn, and a
  **second** WebSocket client on the same listener. It **skips** (never fudges a
  pass) with no binary.

Still **to confirm on a pinned-codex host** before rollout (owned with AGE-198):

1. That `codex --remote <endpoint> resume <thread-id>` attaches to the *running*
   server/thread rather than starting its own process — the central attach claim.
   Do not claim native attach until a host run confirms it.
2. Fresh-session attach flow: `thread/resume` before the first turn fails ("no
   rollout found"); the create-first-turn-then-attach path needs a host test.
3. Last-subscriber thread-unload grace (idle unload after the last client leaves),
   which bounds how long a background session survives with no client.
