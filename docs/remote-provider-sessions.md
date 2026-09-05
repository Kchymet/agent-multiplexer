# Remote provider: publishing sessions to your orchestrator

Status: the `sessions` feature (§1–§3) and the `runtime-events` feature (§4) are
both **implemented** (opt-in; see [Configuration](#6-configuration)). A daemon
advertises `runtime-events` only when it is enabled *alongside* `--publish-sessions`
(it streams transcripts for the sessions `sessions` publishes). The steering
verbs (§3.1) are part of the `sessions` feature and are implemented, delivered by
keystroke to the agent's PTY; a daemon that does not implement a verb answers
`unsupported verb` (§3.2). Extends `docs/remote-provider.md`.

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

### 2.1 Per-session runtime and capabilities

Two additive fields let a consumer gate its UI on what a session actually is and
what its daemon can actually do, rather than assuming a runtime or inferring
control support from the mere existence of a transcript:

```json
{"id":"a2","title":"idempotency","kind":"codex","runtime":"codex",
 "state":"running","status":"running · api",
 "caps":{"prompt":true,"interject":true,"cancel":true,"permission":true}}
```

- `runtime` names the runtime backing the session — `claude` or `codex` (the
  `Runtime*` set of §4). It is set only on rows that are a real agent, never on
  the structural container rows (a repo header, a workgroup root), whose `kind`
  is a layout label. A consumer picks the transcript renderer and the affordance
  set for this value.
- `caps` is the honest control surface for the session — one boolean per steering
  verb (§3.1): `prompt`, `interject`, `cancel`, `permission`. Each says whether
  the daemon can serve that verb for *this* session, so a consumer disables an
  affordance it would only fail on.
  - `permission` is deliberately **not** "a transcript exists". It is true only
    when the runtime raises *correlated* `permission_request` events — a
    `request_id` the `permission` verb can quote back and the daemon can match to
    an open prompt (§3.1, §4.5). A runtime that streams a transcript but records
    no answerable prompt reports `permission:false`.

**Backward compatibility.** Both fields are additive (`omitempty` on `runtime`, a
nullable object for `caps`). A provider that predates them omits both. A consumer
MUST then fall back conservatively: resolve the runtime from `kind` (which has
always carried the same string for agent rows), and — because it cannot know the
per-session control surface — apply its own safe default rather than assuming a
verb works. The nullable `caps` is what makes this decidable: absent (`null`)
means "unknown, fall back"; present with a flag `false` means "known, and this
verb is off".

**Release/consumer dependency order.** These wire types live in the published
`harnessproto` module. A consumer in another repo (the harness orchestrator)
pins `harnessproto` by version, so the ordering is: (1) the producer change —
`harnessproto` plus the amux capability producer — merges and the module commit
is available; (2) the consumer change bumps its `harnessproto` pin to that commit
and reads the new fields. The two ship as separate PRs; until step (1) is merged,
a consumer that wants to build against the new fields pins the producer branch
commit and re-pins to the merged commit before its own merge.

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
| `accepted` | the verb was validated and taken on; the effect is asynchronous — watch `runtime-events` (§4) or the session's `state` for it. Every steering verb (§3.1) is accepted. |

`accepted` promises less than "delivered". A `prompt` to a **stopped** agent is
answered as soon as the daemon knows it is deliverable, and the cold start it
needs happens afterwards (§3.1) — so an accepted verb may not have reached the
runtime yet at all.

```json
{"type":"session-result","reqId":"r7","ok":true,"result":"accepted","accepted":true}
```

`accepted` is the boolean form of `result:"accepted"`, set on exactly the same
replies and never on its own. It exists because "did this succeed?" and "is the
work finished?" are now different questions, and a consumer that must not block
on the second should be able to ask the first without matching against a
disposition vocabulary that may grow.

Both fields are additive and `omitempty`: absent `result` means `applied`, so a
daemon predating them reads correctly and a bare `{"ok":true}` is unchanged on
the wire. Neither is set on a failure — `error` carries that.

### 2.2 Control mode

`caps` says *which* steering verbs work; a third additive field, `controlMode`,
says *how* they are delivered and where the session's runtime events come from.
It is orthogonal to `caps` and does not change how a consumer sends a verb — the
`session-action` route is identical in every mode — but it lets a consumer trust
a structured session's correlated permissions without the transcript-heuristic
caveat, and surface a mode-specific affordance (e.g. "attach a native CLI").

```json
{"id":"a2","kind":"codex","runtime":"codex","controlMode":"structured",
 "caps":{"prompt":true,"interject":true,"cancel":true,"permission":true}}
```

| `controlMode` | delivery | event source |
| --- | --- | --- |
| `pty` (or absent) | keystrokes typed into the agent's PTY (§3.1) | the runtime's on-disk transcript, tailed (§4) |
| `structured` | JSON-RPC to an amux-supervised runtime App Server | that server's live structured stream (§4), normalized to the same event vocabulary |

**Backward compatibility.** `controlMode` is additive (`omitempty`). A provider
that predates it omits it, and a consumer MUST read an empty value as `pty` — the
original keystroke path — so nothing regresses. An unknown future value is also
treated as `pty` (the conservative baseline).

**Structured mode (AGE-181, Codex App Server).** amux runs a background
`codex app-server` per session on a per-session Unix socket inside the session's
private sandbox scope, and is itself the JSON-RPC client. The server's lifetime is
owned by amux — never a UI pane — so closing the native Codex TUI or a remote
client never stops the server or an in-flight turn. The server/thread identity is
persisted by amux (socket path + thread id) so a daemon restart reconnects and
resumes the same thread; it is deliberately **not** published on the wire, because
the socket lives in the private scope. A remote consumer reaches the session
through this contract exactly as for a pty session; a native CLI attaches locally.
The design, lifetime, native-CLI attach syntax, and validation posture are in
`docs/codex-app-server-supervision.md`.

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
| `prompt` | `text` | Deliver a new user turn to the session's agent. If the agent is not running, the daemon MAY start it with `text` as its initial prompt (the `start` path with a prompt) rather than failing, and MAY answer before that start finishes. |
| `interject` | `text` | Deliver text to the agent *while a turn is running* — a steer, not a new turn. |
| `stop` | — | Interrupt the current turn **without killing the session**. The agent stays alive and ready for the next verb; this is not `kill`. |
| `permission` | `request_id`, `decision`, `reason?` | Resolve a permission request the runtime surfaced as a `permission_request` event on the `runtime-events` stream (§4). `request_id` echoes that event's `request_id`; `decision` is `allow` or `deny`; `reason` is optional free text. |

`id` names the target session for all four, and is required. `decision` accepts
exactly `allow` or `deny` — a daemon MUST reject any other value rather than
guess at a permission prompt.

`request_id` is correlated, not decorative. The daemon matches it against the
requests the runtime actually has open — the ones it published as
`permission_request` events (§4.5) — and refuses an id that names none of them
with `ok:false, error:"permission: no pending request …"`. That refusal is the
point of the field: if the turn moves on between the orchestrator seeing a prompt
and its `permission` arriving, the keystroke would otherwise land on a *different*
prompt, allowing or denying an action nobody decided on. A refused verb is
recoverable — re-read the stream and answer the request that is open now.

An **empty** `request_id` still answers whatever prompt is open. That is the
older, uncorrelated behavior, kept as the explicit way to say "whatever it is
asking, allow it"; a caller that can name the request should.

Steering is asynchronous by nature: writing a prompt to a running agent does not
wait for the turn it starts. A successful steering result is therefore
`result:"accepted"` — the daemon took the verb on — and the observable effect
arrives later on the `runtime-events` stream (§4) or as a change of the session's
`state` in the next inventory snapshot (§2). A steering verb that could not be
delivered (no such session, agent not running, unparseable `decision`, no pending
request for that `request_id`) returns `ok:false` with a human-readable `error`.

**A `prompt` that starts a stopped agent acks first.** Everything that can refuse
the verb is decided synchronously — the session exists, its kind is steerable,
the verb is allowed, the `request_id` names an open prompt — and the reply goes
out as soon as those hold. Only then does the daemon start the runtime and type
the text in. This matters because a runtime cold start takes seconds (a Claude
Code boot is the common case, since an idle session is the ordinary thing to
prompt), which is longer than the dispatch timeouts sitting in front of this
relay: a daemon that answered only once the agent was up would fail the request
at the client while succeeding on the machine.

The consequence is that a failure *after* the ack has nowhere to go in the reply.
It is reported instead as an `error`-level `notice` on the session's
`runtime-events` stream (§4.6), alongside the `notice` that says the start began.
A consumer that acts on `accepted` must therefore watch that stream — the ack
means "this will be attempted", not "this worked".

Delivery mechanism is the daemon's business, and v1 of this extension delivers by
writing to the agent's PTY: `prompt`/`interject` write the text followed by
Enter, `stop` sends the runtime's own interrupt key, and `permission` sends the
keystrokes that runtime's permission prompt expects. That is an implementation
detail of the daemon, not the wire — the orchestrator sends the same four verbs
regardless of how a given runtime is driven, and a daemon delivering them over a
runtime's API instead is still conforming.

In amux the keystrokes are per-agent-kind data on the harness registry
(`internal/agent`, `Harness.Keys`), not a switch in the daemon, so adding a
runtime means describing its keys rather than editing the delivery path. Today:

| | submit (`prompt`/`interject`) | `stop` | `permission` allow | `permission` deny |
| --- | --- | --- | --- | --- |
| Claude Code | `Enter` (queued when a turn is running) | `Ctrl+C` | `Enter` on the focused **Yes** | `Esc` (its documented decline) |
| Codex | `Enter` (steers the running turn) | `Esc` (`chat.interrupt_turn`) | `y` (`approval.approve`) | `n` (`approval.decline`) |

Three consequences a consumer should know.

*These are each CLI's default bindings, and both are user-rebindable* (Claude
Code's `keybindings.json`, Codex's `tui.keymap`), so a daemon whose runtime has
been rebound steers wrong. The wire verb stays abstract precisely so that remains
a local concern. Claude Code's interrupt is the sharp case: the documented
interrupt key is `Esc`, but with `"editorMode": "vim"` set, `Esc` is swallowed by
the composer to enter vim NORMAL mode and the turn keeps running — so amux sends
`Ctrl+C`, which interrupts in both modes.

*`stop` can be refused on a live agent.* Claude Code's `Ctrl+C` exits the CLI on
a second press at an idle prompt, so amux sends it only while the agent is
actually mid-turn and answers `ok:false` otherwise — a session must not be killed
by the verb whose contract is to keep it alive. There is no turn to stop in that
state anyway, so the refusal is also the honest answer.

*`prompt` is the only verb that starts a stopped agent.* `interject`, `stop` and
`permission` all act on a turn that must already be in flight, so on a stopped
session they fail rather than surprise the caller by launching one.

*The console is a session like any other.* The built-in `amux console` (`id:
"console"`) is published in the inventory and takes every verb an agent does:
`prompt` starts it when stopped, and `start`/`stop`/`interject`/`permission`
behave as above. It is synthetic on the daemon side (not a stored workgroup or
agent), which is the daemon's business — an orchestrator addresses it by the id
it was published under.

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
  `tool_result`, `plan`, `usage` (`meta`), `permission_request`,
  `permission_resolved`, `notice` (`meta`), `turn_end`, and `raw`. `raw` carries
  `{runtime, native_type, body}` and is the passthrough for any record entry the
  reader has no mapping for — **never dropped**.
- `permission_request` carries `{request_id, tool, action, options}` and says the
  session is blocked on a prompt; `permission_resolved` carries
  `{request_id, decision}` and retires it. Both use the `request_id` as `item_id`,
  so a consumer coalesces the pair into one card. A request is answerable — by the
  `permission` verb (§3.1) — from the moment it is published until its
  `permission_resolved` arrives, and never after. `decision` is `allow`, `deny`,
  or `cleared`: the last means amux knows the prompt closed (the turn ended) but
  not which way it went.
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

Permission prompts are **not** in that table because they are not in the
transcript: see §4.5.

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
| `event_msg` `exec_approval_request` / `apply_patch_approval_request` / `request_user_input` | `permission_request` (`request_id` = `call_id`) |
| `event_msg` `exec_command_begin` / `patch_apply_begin` / `mcp_tool_call_begin`, for a call with an approval open | `permission_resolved` (`allow` — the tool started, so it was approved) |
| `response_item` `function_call_output` for a call with an approval still open | `permission_resolved` (`deny` — the call produced output without ever starting) |
| a turn boundary with an approval still open | `permission_resolved` (`cleared`) |
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

Codex writes its approval prompts into the rollout, so its `permission_request`
events come from the transcript reader alone — no journal (§4.5). It does not
record the *answer*, so the resolution is inferred from what happens to the same
`call_id` next, as the table above says.

Codex records a tool's effect only as that tool's own output — it has no
per-call before/after the way Claude's `Edit`/`Write` inputs give — so a Codex
`tool_result` carries an empty `diffs` list rather than a guess.

### 4.5 Permission prompts (the second record)

Claude Code answers permission prompts in its TUI and writes none of them to the
transcript: the prompt opens, the human picks, and nothing reaches disk. A reader
of the transcript alone therefore cannot see that a session is blocked, and an
orchestrator has no `request_id` to quote back at the `permission` verb (§3.1).

So amux produces the record itself. Claude Code's hooks — which amux already
installs per agent — carry the prompt's whole lifecycle, and each one appends a
line to a per-session **permission journal** (`<state>/permissions/<id>.jsonl`):

| Claude hook event | journal line |
| --- | --- |
| `PermissionRequest` (fires just before the prompt is drawn) | opens a request: a fresh `request_id`, the tool, and a one-line summary of what it wants |
| `PostToolUse` (the tool ran, so its prompt was allowed) | resolves it `allow` |
| `PermissionDenied` | resolves it `deny` |
| `Stop` / `SessionEnd` (a turn cannot end with a prompt up) | resolves anything still open `cleared` |

The journal is read as a **second source** of the same session's stream: the
tailer polls it alongside the transcript, under one shared ordinal space, so
`permission_request` and `permission_resolved` arrive interleaved with the rest
of the conversation and a consumer resumes both with one `afterSeq`. The
consequence for a resync (§4.2) is that a rotation of *either* file restarts
both, since the ordinals are shared.

The `options` a Claude `permission_request` offers are the ones amux can actually
deliver to the prompt (`allow`, `deny`) — not every choice the TUI draws. A
consumer must not be shown a button the daemon has no keystroke for.

A runtime that records its own prompts needs none of this: Codex's rollout
carries them, so its `permission_request` events come from the transcript reader
and there is no journal (§4.4).

Degradation is honest at every step. A session whose hooks are not installed
simply has no journal, so it publishes no `permission_request` — and, since the
`permission` verb refuses an id it cannot find open, a consumer learns that by
being refused rather than by having a keystroke land somewhere unintended. An
upstream rename of one of the resolving hooks leaves a request open until the
turn boundary clears it; a contract test pins the event names so the drift is
visible.

### 4.6 amux's own journal (the third record)

Some of what happens to a session is amux's doing, not the runtime's, and the
runtime therefore never records it. The case that forced this record: a `prompt`
to a stopped agent is acknowledged immediately and the cold start happens
afterwards (§3.1), so "starting agent" — and a start that failed — have no reply
left to travel in.

amux keeps a per-session append-only JSONL journal (`<state>/journal/<session
id>.jsonl`) and the reader tails it as a further source alongside the transcript
and the permission journal. Each line is a `{level, text}` record and becomes one
`notice` event:

| journal line | event |
| --- | --- |
| `{"level":"info","text":"starting agent"}` | `notice` with `level: "info"` — the deferred start began |
| `{"level":"error","text":"start agent a1: …"}` | `notice` with `level: "error"` — the start failed after the verb was acked |
| anything unparsable | `raw` (never dropped) |

Two properties matter to a consumer:

- **It is keyed by the amux session id, not the runtime's conversation id.** The
  first line is written before the runtime — and therefore the conversation —
  exists, which is exactly when a conversation id is unavailable.
- **A session with no transcript still has a stream.** A session that has never
  run resolves to a record naming only this journal, and subscribing to it works.
  Answering "no record" for that session would leave the one case that most needs
  progress reporting with nothing at all.

The journal is polled last of a session's sources, so the shared event ordinals
of everything that predates it are unchanged and a resuming `afterSeq` cursor
still points where it did.

## 5. Compatibility

- These are additive messages behind feature negotiation — protocol version 2
  is unchanged, and peers that don't negotiate the features never see them.
- The `session-action` verb set is likewise additive: new verbs land without a
  version bump, and a daemon that predates one answers `"unsupported verb"`
  (§3.2). `session-result.result` and `session-result.accepted` are additive in
  the same way — absent `result` means `applied`, and a consumer that ignores
  both reads a success exactly as it did before (§2).
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

Every relayed verb leaves one line on `amux provide`'s stderr, before the reply
is written, so an operator can answer "did it reach this machine, and how long
did it take here?" without instrumenting anything:

```
amux provide: session-action prompt id=a1 accepted in 4ms
amux provide: session-action add-agent id=wg1 applied in 21ms (created a7)
amux provide: session-action spawn id=a1 failed in 0s: unsupported
```

The line names the session, the verb, the disposition (§2) and the elapsed
handling time. It deliberately carries no `fields` — that is where the prompt
text and a permission decision's reason live, and the user's words do not belong
in a log just because they passed through here.

With `--runtime-events` (which requires `--publish-sessions`), the daemon also
advertises `runtime-events` and, for each published session the orchestrator
subscribes to, tails that session's on-disk runtime record and streams §4 events
stamped with the runtime that wrote it. The record is resolved through the
session's own harness — Claude Code's session JSONL, keyed by the conversation id
the daemon pins per session, or Codex CLI's rollout, keyed by the pinned rollout
uuid — so a mixed rail streams both. A session with no record on disk, or one
whose runtime has no reader, emits nothing. It is strictly read-only — no input
path, no pane.
