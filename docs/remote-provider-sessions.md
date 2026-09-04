# Remote provider: publishing sessions to your orchestrator

Status: the `sessions` feature (§1–§3) and the `runtime-events` feature (§4) are
both **implemented** (opt-in; see [Configuration](#6-configuration)). A daemon
advertises `runtime-events` only when it is enabled *alongside* `--publish-sessions`
(it streams transcripts for the sessions `sessions` publishes). The steering
verbs (§3.1) are part of the `sessions` feature; a daemon that does not implement
them answers `unsupported verb` (§3.2). Extends `docs/remote-provider.md`.

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

`result` is the disposition of a *successful* verb, and distinguishes a verb
whose effect is already done from one merely handed to a running agent:

| `result` | meaning |
| --- | --- |
| `applied` | the verb ran to completion; its effect is in the next `sessions` snapshot. Every lifecycle verb (§3) is applied. |
| `accepted` | the verb was delivered to the running agent; the effect is asynchronous — watch `runtime-events` (§4) or the session's `state` for it. Every steering verb (§3.1) is accepted. |

The field is additive and `omitempty`: absent means `applied`, so a daemon
predating it reads correctly and a bare `{"ok":true}` is unchanged on the wire.
It is unset on a failure — `error` carries that.

## 3. Messages: orchestrator → daemon

```json
{"type":"sessions-subscribe"}
{"type":"session-action","reqId":"r7","action":"new-workgroup",
 "id":"","target":"","fields":{"name":"payments-fix","repos":"api,web"}}
```

**Lifecycle verbs**: `new-workgroup`, `add-agent`, `rename`, `archive`,
`unarchive`, `start`. Semantics mirror the daemon's local lifecycle operations;
`fields` carries the same form fields the daemon's own clients send. `id` targets
an existing session (the workgroup for `add-agent`; the agent/workgroup for
`rename`/`archive`/`unarchive`/`start`) and is empty for `new-workgroup`.
Internally `archive`/`unarchive` map to the daemon's explicit `set-archived`
(deterministic, not a toggle) and `start` ensures the agent's engine process is
running. They are all synchronous: a success is `result:"applied"` (§2).

Anything outside the verb set below — including any pane/terminal verb (`spawn`,
`input`, `resize`, `kill`, `pane.*`) — MUST be rejected with
`session-result{ok:false, error:"unsupported"}`. This feature carries no pane
verbs at all: it never opens, reads, or writes a pane of the daemon's own
sessions. (Compute panes the orchestrator itself spawned via `spawn` on the
separate compute-provider path are unaffected.)

Authorization: the connection itself is the credential (registered provider,
token-authenticated at register). The daemon SHOULD additionally gate verbs by
local configuration (e.g. read-only publishing: inventory yes, verbs no).

### 3.1 Steering verbs

The lifecycle verbs manage a session from the outside; these four steer the
agent *inside* one that is already running, so an orchestrator can drive a turn
without a terminal. They are session verbs like any other — still no pane
access, still the daemon's choice of delivery mechanism, still rejectable.

| Verb | `fields` | Meaning |
| --- | --- | --- |
| `prompt` | `text` | Deliver a new user turn to the session's agent. If the agent is not running, the daemon MAY start it with `text` as its initial prompt (the `start` path with a prompt) rather than failing. |
| `interject` | `text` | Deliver text to the agent *while a turn is running* — a steer, not a new turn. |
| `stop` | — | Interrupt the current turn **without killing the session**. The agent stays alive and ready for the next verb; this is not `kill`. |
| `permission` | `request_id`, `decision`, `reason?` | Resolve a permission request the runtime surfaced as a `permission_request` event on the `runtime-events` stream (§4). `request_id` echoes that event's `request_id`; `decision` is `allow` or `deny`; `reason` is optional free text. |

`id` names the target session for all four, and is required. `decision` accepts
exactly `allow` or `deny` — a daemon MUST reject any other value rather than
guess at a permission prompt.

Steering is asynchronous by nature: writing a prompt to a running agent does not
wait for the turn it starts. A successful steering result is therefore
`result:"accepted"` — the daemon delivered the verb to the runtime — and the
observable effect arrives later on the `runtime-events` stream (§4) or as a
change of the session's `state` in the next inventory snapshot (§2). A steering
verb that could not be delivered (no such session, agent not running, unparseable
`decision`, no pending request for that `request_id`) returns `ok:false` with a
human-readable `error`.

Delivery mechanism is the daemon's business, and v1 of this extension delivers by
writing to the agent's PTY: `prompt`/`interject` write the text followed by
Enter, `stop` sends the runtime's own interrupt key, and `permission` sends the
keystrokes that runtime's permission prompt expects. That is an implementation
detail of the daemon, not the wire — the orchestrator sends the same four verbs
regardless of how a given runtime is driven, and a daemon delivering them over a
runtime's API instead is still conforming.

Steering verbs are verbs: `--read-only-sessions` (§6) rejects all four exactly as
it rejects the lifecycle verbs.

### 3.2 Verb negotiation

The verb set grows over time and the daemon at the other end may be older than
the orchestrator. There is no per-verb handshake — negotiation is by response:

- A verb outside the accepted set (any pane/terminal verb, any typo) →
  `session-result{ok:false, error:"unsupported"}`. It is never valid on this
  protocol; do not retry it anywhere.
- A verb *in* the set that this daemon does not implement →
  `session-result{ok:false, error:"unsupported verb"}`. The verb is valid
  protocol and an older daemon simply lacks it. An orchestrator SHOULD degrade
  its UI for that session (hide or disable the control) rather than treat the
  connection as broken, and MAY offer the verb again after the daemon upgrades.

Both errors are exact strings. The protocol version stays 2 — the verb set is
additive within the negotiated `sessions` feature (§5).

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
{"type":"runtime-events","sessionId":"a2","runtime":"codex","seq":41,"events":[
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
- `runtime` names the agent runtime whose record produced the batch — `"claude"`,
  `"codex"`, … The set is open: a consumer that meets an unknown runtime renders
  the events generically rather than dropping them. The field is **additive** and
  omitted by a producer that predates it, so a consumer that needs a runtime
  falls back to its own default when it is absent — it must not assume every
  session is Claude.
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

### 4.4 Codex CLI mapping

The daemon locates a Codex session's rollout from the uuid it pins per session
(`$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`). A rollout line is
an envelope — `{timestamp, ordinal, type, payload}` — around one of a handful of
record kinds. Two of them carry the conversation and they overlap: `response_item`
is the durable, model-facing item stream, and `event_msg` is Codex's UI-facing
event stream, most of which restates a `response_item`. The reader takes
`response_item` as the authority for content and `event_msg` only for what no
item carries:

| rollout record | event(s) |
| --- | --- |
| `response_item` `message`, role `user` (kind `user.text`) | `prompt` (`in`) |
| `response_item` `message`, role `assistant` | `text` (`final`; `item_id` = message id) |
| `response_item` `reasoning` | `thinking` (the summary text) |
| `response_item` `function_call` / `custom_tool_call` / `local_shell_call` | `tool_call` (`item_id` = `call_id`, `kind` from tool name); `update_plan` → `plan` |
| `response_item` `function_call_output` (& friends) | `tool_result` (`status` from the exit code Codex reports in the output) |
| `event_msg` `task_started` / `turn_started` | `turn_start` |
| `event_msg` `task_complete` / `turn_complete` / `turn_aborted` | `turn_end` |
| `event_msg` `token_count` | `usage` (`size` = the model context window) |
| `event_msg` `exec_approval_request` / `apply_patch_approval_request` / `request_user_input` | `permission_request` |
| `event_msg` `error` / `stream_error` / `warning` | `notice` |
| `session_meta` / `compacted` | `notice` |
| anything else (`turn_context`, `world_state`, the context Codex injects as a user turn, a later record kind) | `raw` (never dropped) |

Two deliberate exceptions to "never dropped", both de-duplication of records the
reader already emitted from their `response_item` counterpart: the `event_msg`
subtypes that restate an item (`item_started`/`item_updated`/`item_completed`,
the streaming deltas, the `*_begin`/`*_end` tool pairs) and `token_usage_record`,
which restates `token_count`. Mapping them too would double every message,
reasoning block and command in the transcript. Anything *unrecognized* still
becomes `raw`.

Codex records a tool's effect only as that tool's own output — it has no
per-call before/after the way Claude's `Edit`/`Write` inputs give — so a Codex
`tool_result` carries an empty `diffs` list rather than a guess.

## 5. Compatibility

- These are additive messages behind feature negotiation — protocol version 2
  is unchanged, and peers that don't negotiate the features never see them.
- The `session-action` verb set is likewise additive: new verbs land without a
  version bump, and a daemon that predates one answers `"unsupported verb"`
  (§3.2). `session-result.result` is additive in the same way — absent means
  `applied` (§2).
- A daemon may implement `sessions` without `runtime-events` (status-only
  inventory) — consumers should expect that and render inventory alone.

## 6. Configuration

The feature is off by default. Enable it on `amux provide`:

| Flag | Env | Effect |
| --- | --- | --- |
| `--publish-sessions` | `AMUX_PROVIDER_PUBLISH_SESSIONS=1` | advertise `sessions`, publish inventory, accept lifecycle verbs |
| `--read-only-sessions` | `AMUX_PROVIDER_SESSIONS_READONLY=1` | publish inventory but reject every verb with an error — lifecycle (§3) and steering (§3.1) alike |
| `--runtime-events` | `AMUX_PROVIDER_RUNTIME_EVENTS=1` | additionally advertise `runtime-events`: stream read-only structured transcripts for published sessions from the local runtime's session record (Claude Code and Codex CLI). Requires `--publish-sessions`. |

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
subscribes to, tails that session's on-disk runtime record and streams §4 events
stamped with the runtime that wrote it. The record is resolved through the
session's own harness — Claude Code's session JSONL, keyed by the conversation id
the daemon pins per session, or Codex CLI's rollout, keyed by the pinned rollout
uuid — so a mixed rail streams both. A session with no record on disk, or one
whose runtime has no reader, emits nothing. It is strictly read-only — no input
path, no pane.
