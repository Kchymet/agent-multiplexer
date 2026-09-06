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
`github.com/gorilla/websocket`) and frames each JSON-RPC object as one text message.

**The `Origin` finding, and why gorilla (verified against Codex 0.153.4):** the App
Server's **loopback TCP** and **WSS** listeners reject any request carrying an
`Origin` header with **403** (DNS-rebinding protection); its **unix** listener
tolerates one. `golang.org/x/net/websocket` *always* sends `Origin` (and panics if
it is nil), so it can only reach the unix listener. `gorilla/websocket` sends **no
`Origin` unless we set one**, so the amux client — a same-host or authenticated
cross-host client, not a browser — reaches **every** listener (unix, loopback, wss).
`Config.Origin` can set an allowlisted value when a deployment requires it.

Configurable endpoints (`Config.Endpoint` / `--listen`):

| endpoint | use |
| --- | --- |
| `unix://<amux data>/cx/<session key>/cx.sock` | **default** — outside shared worktrees. Each pane masks the socket tree and mounts only its own session socket directory read-write. The key is a fixed-length hash of the session ID, not an authentication credential. |
| `ws://127.0.0.1:<port>` | loopback, colocated clients — now works (Origin omitted); `codexapp.LoopbackEndpoint()` allocates a free port. |
| `wss://host:port` | cross-machine, **authenticated TLS** — verification never downgraded; a non-loopback `ws://` (no TLS) is refused before dialing. |

The AGE-179 harness pilot keeps its stdio transport (not converted gratuitously).

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

**Daemon-restart semantics + the pre-turn resume hazard.** On restart the daemon
re-supervises the session with a fresh `codex app-server`. It **resumes the pinned
thread only once that thread has run a turn** — i.e. the persisted identity is
`Resumable`, set the moment the first `turn/started` is observed (a rollout now
exists on disk). A thread that was pinned but never ran a turn is **not** resumed;
the supervisor starts fresh. This matters because attempting `thread/resume` on a
not-yet-rolled-out thread returns **"no rollout found"** and, per AGE-198's
real-binary run, a *failed pre-turn resume can leave the thread's first turn never
completing*. Gating on `Resumable` avoids the failed resume entirely; the handshake
also keeps a backstop — if a resume is attempted and still misses (e.g. the rollout
was pruned), it falls back to `thread/start` and adopts the new id (single source of
truth: `Identity.ThreadID == ThreadID()`, so no split). The live event seq space
restarts from 1 per supervisor lifetime, which a consumer dedups by seq like any
tailer resync. Validated end-to-end against the real binary
(`TestSmokeTurnAfterResumeMiss`: a first turn completes after a pre-turn resume miss).

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

## Validation posture

Unit tests run with no binary (in-memory fake App Server): handshake start/resume
incl. the empty-thread `thread/start` fallback, observed turn bracketing from any
origin, interject, cancel, approval round-trip with the `{decision}` object +
stale/duplicate rejection + no-speculative-resolve, user-input not auto-answered,
disconnect-mid-turn, unknown request; a real WebSocket handshake + round-trip over
unix and loopback TCP + the non-loopback refusal (`wsconn_test.go`); and the daemon
integration (structured verb routing + refusals, opt-in gate, durable event-record
identity mapping + permission replay, control-mode stamp). Legacy PTY steering
tests pass unchanged.

**Validated against the real binary (Codex 0.153.4), opt-in via
`AMUX_CODEX_APP_SERVER_SMOKE=1`:**

- `TestSmokeRealAppServer` — WebSocket-over-unix handshake, `on-request` /
  `workspace-write` accepted, thread id returned, a turn **bracketed** (turn_start …
  turn_end from the observed notifications — confirming the method names), and a
  **second client completing its own `initialize`** on the same listener. Model
  outcome is *reported* (without credentials the turn ends `failed`), never passed
  off as success.
- `TestSmokeEmptyThreadResume` / `TestSmokeTurnAfterResumeMiss` — a pinned empty
  thread's `thread/resume` returns "no rollout found"; the supervisor adopts a new
  thread as the single source of truth (Identity/ThreadID agree, no split, no lost
  conversation) **and a first turn completes after that resume miss** — the failed
  resume does not poison the turn.
- `TestSmokeLoopbackTransport` — the amux client (Origin omitted) completes the
  handshake against codex's **loopback ws** listener, which 403s any Origin (the
  concrete reason for the `gorilla/websocket` switch).
- `TestSandboxedAppServerLaunch` — an **actual bwrap launch** of the argv
  `panespec.AppServerCommand` produces: the codex executable is reachable inside the
  scope and its separately mounted Unix endpoint is reachable across the
  sandbox boundary (handshake succeeds). Socket isolation is tested separately.

Also confirmed against the schema/CLI: `--listen` accepts `stdio://`/`unix://`/
`ws://IP:PORT`/`off`; `--remote <ADDR>` is a real global TUI option (with
`--remote-auth-token-env`); the approval response is `{"decision":"accept"|
"decline"}`; `requestUserInput` answers are a map by question id.

**Still requires an interactive host run (owned with AGE-198):**

1. That a *native Codex TUI* launched `codex --remote <endpoint> resume <thread-id>`
   attaches to the running server/thread and shares it live with the web bridge —
   the amux tests prove the server, socket, and multi-client `initialize`, but not a
   real TUI attaching and co-driving one thread.
2. Last-subscriber thread-unload grace (idle unload after the last client leaves).

### Private App Server socket mounts

A read-only bind of a Unix socket does not prevent connecting to it. App Server
sockets therefore live under `<amux data>/cx/<session key>/`, outside the shared
session/workgroup directories. After all runtime, data, worktree, and config
mounts, every pane overlays the socket root with an empty tmpfs and mounts only
its own session's socket directory read-write. If the data directory is a
symlink, its canonical socket-root path is masked too. Directory creation or
binding failures must not omit the mask and expose sibling endpoints.

The own directory exists before a pane launches, so a terminal opened before its
App Server can connect when that server starts. The parent mask also hides peers
created after the pane starts. Coordinator access to workgroup files does not
implicitly grant access to members' raw App Server sockets.

`TestScopeRejectsSiblingSessionSocket` runs real connections in bubblewrap. It
checks own access, denied sibling/canonical/proc-alias access, coordinator own vs
peer access, and own/peer sockets created after namespace setup. Denial must be a
missing-path or permission error, not a broken listener or oversized address.
`TestSandboxedAppServerLaunch` still proves the real Codex handshake across the
own socket bind.

These checks establish the tested socket mount boundary. They do not turn amux
into a hostile multi-tenant security boundary: network/PID sharing, inherited
identity/configuration, and access to the amux control API require their own
policies. Full native/provider/browser acceptance remains a separate gate.
Existing servers keep their endpoints until they restart; new launches use the
new location and preserve the persisted thread's resume behavior. No rollout
flag is enabled by this change.
