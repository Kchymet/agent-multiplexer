# Remote provider: publishing sessions to your orchestrator

Status: the `sessions` feature (§1–§3) and the `runtime-events` feature (§4) are
both **implemented** (opt-in; see [Configuration](#6-configuration)). A daemon
advertises `runtime-events` only when it is enabled *alongside* `--publish-sessions`
(it streams transcripts for the sessions `sessions` publishes). Extends
`docs/remote-provider.md`.

Provider mode (`amux provide`) lets a remote orchestrator use this machine as
compute: it spawns panes here and streams their I/O. This document specifies an
**optional, additive extension** on the same dialed connection: publishing this
daemon's own *session inventory* (workgroups and agents) to the orchestrator,
and accepting a small set of lifecycle verbs back — so an orchestrator's UI can
show and manage your local sessions remotely.

Everything below preserves provider mode's trust model:

- **The daemon owns the connection.** All messages ride the existing dial-out
  TLS connection. The orchestrator never dials in; no new listener is opened.
- **Opt-in per feature.** Nothing here is sent unless negotiated (§1).
- **The daemon is authoritative.** Every verb may be rejected; the daemon
  enforces its own policy regardless of what the orchestrator asks.
- **No terminal access.** This extension carries *no* pane verbs. The
  orchestrator cannot open, read, or write panes of the daemon's own sessions.
  (Compute panes the orchestrator itself spawned via `spawn` are unaffected.)

## 1. Negotiation

Two independent feature strings in `register.capabilities.features`:

- `"sessions"` — the daemon will publish its session inventory and accept
  session lifecycle verbs.
- `"runtime-events"` — the daemon can additionally stream structured
  transcript events for sessions it publishes (§4).

Once a feature is negotiated, both peers MUST ignore message types they don't
recognize (forward compatibility). Without negotiation these messages are
never sent; a conforming peer should not send them, and the daemon simply
ignores a stray `sessions-subscribe` / `session-action` when the feature is
inactive (a lenient superset of "treat as a protocol error").

Negotiation completes in two steps: the daemon lists `sessions` in
`register.capabilities.features`, and the orchestrator opts in by sending
`sessions-subscribe` (§3). The daemon publishes nothing until it receives that
subscribe — the subscribe is the orchestrator's ack.

## 2. Messages: daemon → orchestrator

One JSON object per line, same `wire` framing as all provider traffic.

```json
{"type":"sessions","seq":12,"sessions":[
  {"id":"a1","title":"payments-fix","rootId":"","isRoot":true,
   "section":"workgroups","repos":"api,web","mode":"task",
   "state":"running","status":"running · 2 agents",
   "startedAt":1751500000,"archived":false},
  {"id":"a2","title":"idempotency","rootId":"a1","isRoot":false,
   "section":"workgroups","repos":"api","mode":"task",
   "state":"waiting","status":"waiting · needs input",
   "startedAt":1751500100,"archived":false}
]}
```

- Full-snapshot semantics: each `sessions` frame replaces the previous one
  (marshal-and-compare on the daemon side; push on change plus on subscribe,
  debounced at a poll cadence — one second by default).
- `seq` is per-connection monotonic, from 1; a receiver drops frames with stale
  seq. A reconnect starts a fresh sequence and re-publishes a full snapshot.
- Each element is the daemon's normalized session model (`core.Session`), so the
  wire carries the full field set — the illustrative example above shows the
  load-bearing subset. Field vocabulary: `section` ∈
  `workgroups | repos | detached | archived`; `state` ∈
  `idle | ready | waiting | running | unknown` (the attention ladder). `archived`
  is emitted only when true (JSON `omitempty`); an archived session also carries
  `section:"archived"`.
- The daemon MAY redact sessions (e.g. publish only non-archived, or nothing
  at all while still advertising the feature) — inventory content is policy.

```json
{"type":"session-result","reqId":"r7","ok":true,"newId":"a9","error":""}
```

Response to a `session-action`, correlated by `reqId`. `newId` is set for
creation verbs (`new-workgroup`, `add-agent`); `ok` and `error` follow JSON
`omitempty`, so a bare success is `{"type":"session-result","reqId":"r7","ok":true}`
and a failure carries `ok:false` with a non-empty `error`.

## 3. Messages: orchestrator → daemon

```json
{"type":"sessions-subscribe"}
{"type":"session-action","reqId":"r7","action":"new-workgroup",
 "id":"","target":"","fields":{"name":"payments-fix","repos":"api,web"}}
```

Verbs (v1): `new-workgroup`, `add-agent`, `rename`, `archive`, `unarchive`,
`start`. Semantics mirror the daemon's local lifecycle operations; `fields`
carries the same form fields the daemon's own clients send. `id` targets an
existing session (the workgroup for `add-agent`; the agent/workgroup for
`rename`/`archive`/`unarchive`/`start`) and is empty for `new-workgroup`.
Internally `archive`/`unarchive` map to the daemon's explicit `set-archived`
(deterministic, not a toggle) and `start` ensures the agent's engine process is
running. Anything else — including any pane/terminal verb (`spawn`, `input`,
`resize`, `kill`, `pane.*`) — MUST be rejected with
`session-result{ok:false, error:"unsupported"}`. This feature carries no pane
verbs at all: it never opens, reads, or writes a pane of the daemon's own
sessions. (Compute panes the orchestrator itself spawned via `spawn` on the
separate compute-provider path are unaffected.)

Authorization: the connection itself is the credential (registered provider,
token-authenticated at register). The daemon SHOULD additionally gate verbs by
local configuration (e.g. read-only publishing: inventory yes, verbs no).

## 4. Structured transcript events (`runtime-events`)

When negotiated, the daemon streams structured events for a published session —
derived by a daemon-side reader of the local runtime's on-disk session record —
so the orchestrator can render a transcript without any PTY access.

### 4.1 Subscribe (orchestrator → daemon)

The orchestrator opts in per session, resuming from a cursor:

```json
{"type":"runtime-events-subscribe","sessionId":"a2","afterSeq":40}
```

The daemon streams nothing for a session until it receives this. `afterSeq` is a
resume cursor: the daemon emits only events whose ordinal exceeds it (`0` = from
the start). A daemon with no structured record for the named session simply emits
nothing for it (honest degradation) — the feature stays advertised.

### 4.2 Events (daemon → orchestrator)

```json
{"type":"runtime-events","sessionId":"a2","seq":41,"events":[
  {"type":"text","item_id":"m3","direction":"out","payload":{"text":"…"}},
  {"type":"tool_call","item_id":"t9","direction":"out","payload":{"title":"edit","input":"…"}}
]}
```

- The event envelope is intentionally generic: `type`, optional `item_id`,
  optional `direction` (`in`/`out`/`meta`), and an opaque `payload`. Producers
  SHOULD use a stable, documented vocabulary; consumers MUST pass an unknown
  `type` through rather than dropping it.
- **Vocabulary the daemon emits** (a stable set; a consumer maps it onto its own
  model): `turn_start`, `prompt` (`in`), `text`, `thinking`, `tool_call`,
  `tool_result`, `plan`, `usage` (`meta`), `notice` (`meta`), `turn_end`, and
  `raw`. `raw` carries `{runtime, native_type, body}` and is the passthrough for
  any record entry the reader has no mapping for — **never dropped**.
- `seq` is per-session monotonic — the ordinal of the **last** event in `events`;
  the batch is ascending, so the first event's ordinal is `seq − len(events) + 1`.
  A consumer resumes by subscribing with `afterSeq` = the highest ordinal it has
  stored. Ordinals are assigned deterministically by record position, so a
  consumer that keys on the ordinal ingests a re-sent prefix idempotently.
- Read tolerance: the record file may not exist yet (nothing is emitted until it
  appears), grows by append (only new complete lines are read; a partial trailing
  line waits for its newline), or is rotated/truncated (detected by inode change
  or a size shrink; the reader restarts from the top).
- Streaming is read-only by definition; there is no input counterpart. It never
  opens, reads, or writes a pane.

### 4.3 Claude Code mapping (the first runtime)

The daemon locates a Claude Code session's transcript from the conversation id it
pins per session (`<projects>/<munged-cwd>/<uuid>.jsonl`) and maps each JSONL
record to the vocabulary above:

| JSONL record | event(s) |
| --- | --- |
| `user`, string content | `turn_start` + `prompt` (`in`) |
| `user`, `tool_result` block | `tool_result` (file diffs recovered from the matching `tool_use` input) |
| `assistant`, `text` block | `text` (`final`; `item_id` = message id + block index) |
| `assistant`, `thinking` block | `thinking` |
| `assistant`, `tool_use` block | `tool_call` (`kind` from tool name); `TodoWrite` → `plan` |
| `assistant` `message.usage` | `usage` |
| `assistant` non-tool `stop_reason` | `turn_end` |
| `system` / `summary` | `notice` |
| anything else (unparsable, or `mode`/`ai-title`/… state records) | `raw` (never dropped) |

Codex's session/rollout files are a later runtime under the same envelope.

## 5. Compatibility

- These are additive messages behind feature negotiation — protocol version 2
  is unchanged, and peers that don't negotiate the features never see them.
- A daemon may implement `sessions` without `runtime-events` (status-only
  inventory) — consumers should expect that and render inventory alone.

## 6. Configuration

The feature is off by default. Enable it on `amux provide`:

| Flag | Env | Effect |
| --- | --- | --- |
| `--publish-sessions` | `AMUX_PROVIDER_PUBLISH_SESSIONS=1` | advertise `sessions`, publish inventory, accept lifecycle verbs |
| `--read-only-sessions` | `AMUX_PROVIDER_SESSIONS_READONLY=1` | publish inventory but reject every verb with an error |
| `--runtime-events` | `AMUX_PROVIDER_RUNTIME_EVENTS=1` | additionally advertise `runtime-events`: stream read-only structured transcripts for published sessions from the local runtime's session record (Claude Code first). Requires `--publish-sessions`. |

With `--publish-sessions`, the published rail is the daemon's own session
inventory — a store-backed poll annotated with engine liveness (read from the
file the running daemon persists, so no second daemon connection is needed to
light up AAP-derived state). Lifecycle verbs run through the local daemon socket
so the daemon stays authoritative (it owns the engine that `start` needs and the
re-poll that surfaces a change); if no daemon is reachable, verbs fail cleanly.
Feature strings passed via `--feature`/`AMUX_PROVIDER_FEATURES` are orthogonal
and still advertised alongside `sessions`.

With `--runtime-events` (which requires `--publish-sessions`), the daemon also
advertises `runtime-events` and, for each published session the orchestrator
subscribes to, tails that session's on-disk runtime record and streams §4 events.
For Claude Code it resolves the record from the conversation id the daemon pins
per session; a session with no record on disk emits nothing. It is strictly
read-only — no input path, no pane.
