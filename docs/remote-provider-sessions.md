# Remote provider: publishing sessions to your orchestrator

Status: specified; implementation pending. Extends `docs/remote-provider.md`.

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
never sent, and a conforming peer treats them as a protocol error.

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
  (marshal-and-compare on the daemon side; push on change plus on subscribe).
- `seq` is per-connection monotonic; a receiver drops frames with stale seq.
- Field vocabulary matches the daemon's session model: `section` ∈
  `workgroups | repos | detached | archived`; `state` ∈
  `idle | ready | waiting | running | unknown` (the attention ladder).
- The daemon MAY redact sessions (e.g. publish only non-archived, or nothing
  at all while still advertising the feature) — inventory content is policy.

```json
{"type":"session-result","reqId":"r7","ok":true,"newId":"a9","error":""}
```

Response to a `session-action`; `newId` set for creation verbs.

## 3. Messages: orchestrator → daemon

```json
{"type":"sessions-subscribe"}
{"type":"session-action","reqId":"r7","action":"new-workgroup",
 "id":"","target":"","fields":{"name":"payments-fix","repos":"api,web"}}
```

Verbs (v1): `new-workgroup`, `add-agent`, `rename`, `archive`, `unarchive`,
`start`. Semantics mirror the daemon's local lifecycle operations; `fields`
carries the same form fields the daemon's own clients send. Anything else —
including any pane/terminal verb — MUST be rejected with
`session-result{ok:false, error:"unsupported"}`.

Authorization: the connection itself is the credential (registered provider,
token-authenticated at register). The daemon SHOULD additionally gate verbs by
local configuration (e.g. read-only publishing: inventory yes, verbs no).

## 4. Structured transcript events (`runtime-events`)

When negotiated, the daemon may stream structured events for a published
session — e.g. derived from an agent runtime's on-disk session record — so the
orchestrator can render a transcript without any PTY access:

```json
{"type":"runtime-events","sessionId":"a2","seq":41,"events":[
  {"type":"text","item_id":"m3","direction":"out","payload":{"text":"…"}},
  {"type":"tool_call","item_id":"t9","payload":{"name":"edit","input":{…}}}
]}
```

- The event envelope is intentionally generic: `type`, optional `item_id`,
  optional `direction`, and an opaque `payload`. Producers SHOULD use a stable,
  documented vocabulary; consumers MUST pass unknown event types through rather
  than dropping them.
- `seq` is per-session monotonic so a consumer can resume (`runtime-events-
  subscribe {sessionId, afterSeq}`) without replaying everything.
- Streaming is read-only by definition; there is no input counterpart in this
  extension.

## 5. Compatibility

- These are additive messages behind feature negotiation — protocol version 2
  is unchanged, and peers that don't negotiate the features never see them.
- A daemon may implement `sessions` without `runtime-events` (status-only
  inventory) — consumers should expect that and render inventory alone.
