# Codex App Server supervision (AGE-181)

Status: **opt-in, experimental.** Default Codex control stays the PTY path
(keystroke steering + rollout tailing). This document describes the structured
control mode amux offers when it supervises a Codex App Server itself, and the
parts of it that still need host validation before rollout.

## What it is

For a Codex session in **structured** control mode (`controlMode:"structured"`,
docs/remote-provider-sessions.md §2.2), amux runs a background `codex app-server`
process **per session** on a per-session Unix socket, and is itself the JSON-RPC
client. Both the native Codex TUI in the amux pane and a remote consumer drive the
**same server/thread**:

- the remote consumer reaches the session through the existing provider contract
  (`sessions` + `runtime-events` + `session-action`) — it never touches the socket;
- the native CLI attaches **locally**, inside the sandbox, with
  `codex --remote unix://<socket> resume <thread-id>`.

The App Server speaks the protocol validated by the AGE-179 harness pilot
(`initialize` → `thread/start`|`thread/resume` → `turn/start`|`turn/steer`|
`turn/interrupt`, server-initiated `.../requestApproval` requests), so amux's
client and a native `--remote` peer are interchangeable clients of one server.

## Lifetime is independent of any UI pane

The single most important property: **the App Server's lifetime is owned by amux,
bound to the daemon's context — never a pane or a client connection.**

- The `codex app-server` child is started in its own process group, so a signal
  aimed at the foreground pane never reaches it.
- Closing the native TUI, or a remote client disconnecting, does **not** stop the
  server or interrupt an in-flight turn. **No process is killed by client
  disconnect.**
- Only `Supervisor.Close()` (session archived/deleted, or leaving structured
  mode) or daemon shutdown terminates the server.

## Socket / thread identity persistence

amux persists, per structured session, `{socket path, thread id, control mode}`
as a JSON sidecar under `<state>/codexapp/<session>.json` (see
`internal/codexapp/identity.go`). This is amux-internal state and is **not** put on
the wire — the socket lives in the private sandbox scope, and connecting an
arbitrary remote client to it would break identity/sandbox scope.

**Daemon-restart semantics.** On restart the daemon reads the sidecar and
re-supervises the session: it starts a fresh `codex app-server`, dials the socket,
and runs the handshake with `thread/resume <thread-id>` so the conversation
continues on the same thread. The live runtime-event seq space restarts from 1 (a
new supervisor lifetime), which a consumer dedups by seq exactly as it does for an
on-disk tailer resync (§4). Within one supervisor lifetime, a reconnecting
subscriber resumes from a bounded in-memory replay ring via its `afterSeq` cursor.

## Steering and events

- **Steer verbs** (`prompt`, `interject`, `stop`, `permission`) route to JSON-RPC
  (`turn/start`, `turn/steer`, `turn/interrupt`, an approval response) instead of
  keystrokes. They remain `accepted` (asynchronous), same as the PTY path.
- **Permissions are natively correlated.** An approval is a server→client JSON-RPC
  request; its id is the `request_id` a `permission` verb echoes back. amux tracks
  outstanding approvals and rejects a **stale** (unknown) or **duplicate**
  (already-answered) reply, and never guesses affirmatively on an unparseable
  decision. When a turn ends, any approval still open is cleared with a
  `permission_resolved{decision:"cleared"}` so a consumer never waits forever.
- **Events** are the App Server's live notification stream normalized to the shared
  runtime-event vocabulary (§4) — the *same* events the rollout tailer produces, so
  a consumer sees one story regardless of control mode. A structured
  request-user-input is surfaced as a `notice` + `raw` and answered with empty
  answers so the turn does not hang (it is not interactively answerable here).

## Opt-in and fallback

Structured mode is opt-in; the default is the untouched PTY/exec path. The two are
never wired at once for a given session. The intended selector is an amux env flag
(`AMUX_CODEX_CONTROL=app-server`, default off) applied at launch; the daemon
routing that consumes it lands in the follow-up PR.

## Validation posture — read this before claiming it works

There is **no codex binary in the amux CI sandbox, and this work installs none.**
Consequently:

- The supervisor's protocol is covered exhaustively by in-memory contract tests
  against a fake App Server (`internal/codexapp/*_test.go`), including handshake
  (start/resume), turn bracketing, interject, cancel, the approval round-trip with
  stale/duplicate rejection, disconnect-mid-turn, and the unknown-request path.
- An **opt-in, self-skipping** real-runtime smoke test
  (`AMUX_CODEX_APP_SERVER_SMOKE=1`, `TestSmokeRealAppServer`) drives a real
  `codex app-server` on a Unix socket when a pinned binary is present, logging the
  exact version and proving the socket binds, the handshake succeeds, a turn is
  bracketed, and a second client can dial the same socket. It **skips** (never
  fudges a pass) when no binary is present.

Still **unvalidated on host** and required before rollout:

1. The exact `codex app-server --listen unix://<path>` flag/scheme on the pinned
   CLI (docs headline `ws://…`; unix support is claimed for 0.153.4 but must be
   confirmed on the host).
2. That `codex --remote unix://<socket> resume <thread-id>` **attaches to the
   running server/thread** rather than starting its own process. This is the
   central AGE-181 attach claim and MUST be confirmed interactively on the host —
   do not claim the native CLI attaches until it is.
3. Last-subscriber thread-unload grace behavior (the App Server docs describe an
   idle unload after the last subscriber leaves), which affects how long a
   background session survives with no client attached.

The argv builders `codexapp.AppServerArgv` and `codexapp.AttachArgv` encode the
documented syntax and carry these caveats in their doc comments.
