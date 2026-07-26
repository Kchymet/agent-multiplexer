# amux Agent Protocol (AAP) v0

A common protocol by which an **agent** reports its state, identity, and progress
to an **orchestration harness** (the amux daemon), and by which the harness drives
the agent. It generalizes the existing Claude-hook status mechanism into an
agent-agnostic, multi-channel contract that any runtime (Claude Code, a shell
script, a custom agent) can implement.

- **Status:** draft / v0.
- **Compatibility:** the current `amux hook <state>` behavior is a strict subset
  of this spec (see [§11 Compatibility](#11-compatibility-with-v0-hooks)). Existing
  agents keep working unchanged.
- **Baseline implementation today:** `internal/core/hookstate.go`,
  `internal/claudecfg/claudecfg.go`, `cmd/amux/main.go` (`cmdHook`),
  `internal/source/workspace.go`, `internal/daemon`.

---

## 1. Design principles

1. **Agents push; the harness never scrapes.** Activity is reported explicitly by
   the agent. No transcript/PTY parsing is used to *infer* meaning. (The harness
   independently observes process *liveness*; see §7.)
2. **Reporting must never disrupt the agent.** A report call always exits `0`,
   swallows its own errors, requires no live daemon, and writes through an atomic
   rename. A down harness loses visibility, never correctness.
3. **Two orthogonal signals, combined by the harness.** *Liveness* (is the process
   running — observed by the harness) gates *activity* (what the agent says it is
   doing — reported by the agent). Liveness always wins: a dead process is `idle`
   regardless of a stale `running` report.
4. **Channels are independent and idempotent.** `status`, `label`, `topic`,
   `progress`, `attention`, and `fields` are separate channels. Updating one never
   clobbers another. Re-reporting the same value is a no-op except for the
   freshness timestamp.
5. **Last-writer-wins, monotonic freshness.** Each report stamps `updated`
   (unix millis). The harness treats the most recent record as truth and may age
   out stale records (§7).

---

## 2. Roles & terms

| Term | Meaning |
|---|---|
| **Agent** | A process doing work in a worktree; the reporter. |
| **Harness / daemon** | The long-lived process that owns agent processes (the *engine*), polls reports, and broadcasts a `Snapshot` to UIs. |
| **Session id** | The stable identity an agent reports under. In amux this is the pinned conversation uuid (`store.Session.ClaudeID`), chosen by the harness at launch. |
| **Rail** | A UI that renders the harness `Snapshot`. A pure consumer; never a party to this protocol. |
| **Record** | The current reported state for one session (§6). |

---

## 3. Identity & addressing

Every report is made **on behalf of a session id**. An implementation MUST resolve
the session id in this precedence order, stopping at the first hit:

1. Explicit `--session <id>` flag (or `session` parameter).
2. `$AMUX_SESSION_ID` environment variable (the harness SHOULD set this in every
   agent process it launches).
3. A field named `session_id` in a JSON object on **stdin** (the Claude Code hook
   binding; see §11).
4. The tmux `@amx_ws` window variable, if the agent runs inside an amux-managed
   tmux window (the path `amux agent name` uses today).

If no id resolves, the report is a **silent no-op** (exit `0`). Reporting MUST NOT
fail the agent merely because identity is unknown.

> **Planned: authenticated identity.** Today identity is *inferred*, not
> *authenticated* — any process that can name a session id can report under it.
> A future revision introduces per-agent identity (authn + authz): the harness
> issues each agent a scoped credential at launch, and the `amux agent` commands
> present it so a report can only be made *by* the agent it concerns. The
> resolution order above is the pre-auth fallback and will remain as the local
> trusted-path default.

> **Why the harness picks the id.** amux mints the conversation id up front and
> pins it onto the agent (`--resume`/`--session-id`), so reports written under that
> id join cleanly to the store row without a registration handshake. Records whose
> id matches no known session are surfaced as **untracked** rows rather than
> dropped.

---

## 4. Transport bindings

The protocol defines one logical API (§5) with three interchangeable bindings.
An agent runtime implements **whichever is convenient**; the harness MUST accept
all three writing to the same record.

### 4a. CLI binding (normative, primary)

The agent invokes the `amux` binary as a subprocess:

```
amux agent <verb> [args...] [--session <id>] [--detail <text>]
```

This is the lowest-common-denominator binding: any runtime that can spawn a
process can report, including via shell hooks. The CLI resolves identity (§3),
applies the update, and exits `0`. It is the binding shell hooks and
language-agnostic agents should use.

### 4b. File binding (normative, underlying contract)

The CLI binding is sugar over a file write that other tools MAY perform directly.

- **Location:** `<StateDir>/reports/<sanitized-session-id>`, where `<StateDir>` is
  `core.StateDir()` (today `~/.local/state/amux`). One file per session; no
  extension; `sanitizeID` maps any char outside `[A-Za-z0-9-_]` to `_`.
- **Format:** the JSON Record (§6), UTF-8.
- **Write discipline:** **read-modify-write merge** — read the existing record,
  overlay only the channels this update touches, set `updated`, then write
  `<path>.tmp` and `os.Rename` over the target. Atomic rename guarantees readers
  never see a torn file.
- **Concurrency:** the sole writer is the agent (its own report calls, which are
  short and serialized in practice); readers are the harness poll loop. Rename
  atomicity is sufficient; no lock is required. If an implementation issues
  concurrent reports, it SHOULD serialize them.

> v0 used `…/hooks/<id>`; v1 uses `…/reports/<id>`. A v1 harness MUST read both
> directories and merge, preferring the newer `updated`. See §11.

### 4c. Socket binding (normative, for live connections)

When a live daemon connection already exists, an agent (or a tool acting for it)
MAY report over the control socket instead of the filesystem, using a `report`
action on the existing newline-delimited-JSON envelope (§9):

```json
{"action":"report","id":"<session-id>","fields":{"status":"running","topic":"wiring oauth"}}
```

The daemon applies it to the same record as the file binding. This binding
requires a running daemon and so is **not** suitable for the
"must-never-disrupt" hot path; prefer 4a/4b for status from inside the agent.

---

## 5. The reporting API (agent → harness)

Language-neutral function surface. Each maps to a CLI verb (4a) and a Record
channel (4b/§6). All functions are **idempotent** and **best-effort** (return
void; never throw to the caller).

### 5.1 `report_status(state, detail?)`

Set the agent's lifecycle/activity state.

- **CLI:** `amux agent status <state> [--detail <text>]`
- **Channel:** `state` (+ optional `detail`)
- **`state` ∈** the enum in §8. Unknown values are stored verbatim but rendered as
  `unknown` by conformant harnesses.
- **`detail`** is a short free-text qualifier (e.g. `"running tests"`); advisory,
  rail MAY show it after the state.
- **Idempotency:** re-reporting the same state only refreshes `updated`.

### 5.2 `set_label(text)`

Set the agent's short human-readable display name (the rail's primary label).

- **CLI:** `amux agent label <text>`  (alias of `amux agent name <text>`)
- **Channel:** `label`
- **Semantics:** maps to the session's display name (`Session.Title`, backed by
  `store.Session.Name`). The harness SHOULD persist `label` durably (equivalent to
  the `rename` action) so it survives restarts; `topic`/`status` need not persist.
- Empty string clears the label (falls back to the id).

### 5.3 `set_topic(text)`

Set a longer free-text description of *what the agent is currently working on*.

- **CLI:** `amux agent topic <text>`
- **Channel:** `topic`
- Distinct from `label`: `label` is a stable name ("auth spike"); `topic` is the
  current focus ("wiring the oauth callback, blocked on redirect URI"). Volatile;
  not persisted across restarts.
- Empty string clears it.

### 5.4 `report_progress(value, total?, detail?)`

Report quantitative progress.

- **CLI:** `amux agent progress <value> [--total <n>] [--detail <text>]`
- **Channel:** `progress = {value, total?, detail?}`
- If `total` is given, `value/total` is a fraction (rail MAY draw a bar). If
  `total` is omitted, `value` is an open-ended counter (e.g. items processed).
- `amux agent progress --clear` removes the channel.

### 5.5 `request_attention(reason)` / `clear_attention()`

Signal that the agent is blocked and needs a human.

- **CLI:** `amux agent attention <reason>` / `amux agent attention --clear`
- **Channel:** `attention = {reason, since}` (`since` = first time set)
- This is the structured form of the `waiting` state's intent. Setting it SHOULD
  also drive `state → waiting` unless the agent set a state explicitly in the same
  call. The rail MUST surface attention with the highest priority (§8 ladder).

### 5.6 `set_field(key, value)` / `clear_field(key)`

Attach arbitrary structured key/values (branch, test counts, cost, queue depth…).

- **CLI:** `amux agent field <key> <value>` / `amux agent field <key> --clear`
- **Channel:** `fields` (a string→string map)
- Keys are namespaced by convention with `.` (e.g. `git.branch`, `tests.passed`).
  The harness treats values opaquely; UIs decide what to show. Bounded: an
  implementation MAY cap the map (RECOMMENDED ≤ 32 keys, ≤ 1 KiB each) and drop
  excess, logging that it did (no silent truncation).

### 5.7 `emit_event(message, level?)`

Append an **ephemeral** activity-log line (not retained state).

- **CLI:** `amux agent event <message> [--level info|warn|error]`
- **Transport:** SHOULD use the socket binding (4c) when available, since events
  are a stream, not state. Over the file binding an implementation MAY keep a
  small bounded ring in the record under `events`; harnesses MAY ignore it.
- Events are advisory and lossy by design; never use them to convey state that a
  channel above can hold.

### 5.8 `heartbeat()`

Refresh `updated` without changing any channel.

- **CLI:** `amux agent heartbeat`
- Use for long-running turns so freshness-based staleness (§7) does not misfire.
  Optional: liveness (§7) is the primary anti-staleness signal; heartbeats matter
  for runtimes the engine cannot observe directly.

### 5.9 `report_done()`

Declare the agent's task **complete**. This is a *terminal* lifecycle report, not
an activity state (§8): a one-off agent that has finished its task and integrated
its artifact calls this to retire itself from the active rail.

- **CLI:** `amux agent done`
- **Effect:** archives the agent's own session — it drops off the active rail into
  the ARCHIVED section (§SectionArchived). Reversible: it hides the row, it does
  **not** delete the worktree or branch (`amux workgroup unarchive <id>` restores it).
- **Identity:** unlike the activity verbs, `done` acts on the *store* session id
  (the id the archive/rename control actions take), which the harness sets on every
  launched agent as `$AMUX_WORKGROUP`. Precedence: `--id <id>`, then
  `$AMUX_WORKGROUP`, then its legacy `$AMUX_WORKSPACE` alias. When none resolves
  the call is a silent no-op (the caller isn't an amux-launched agent).
- **Bridges the two planes (like §5.2 `set_label`).** `done` is reported through
  the control plane (the `set-archived` action, §9) because archival is durable
  store state the harness owns, not a volatile activity channel. It remains
  best-effort and MUST NOT disrupt the agent: a missing identity or an unreachable
  harness is reported and swallowed, and the call still exits `0`.
- **Idempotent:** marking an already-done session done again is a no-op.

> Distinct from `status idle` (§5.1/§8): `idle` means "no live turn right now" and
> flips back to `running` on the next message; `done` means "this session's job is
> finished" and takes it off the active rail until a human restores it. Use `done`
> only for genuinely task-driven, one-off sessions — never for an interactive
> session (the human ends it) or a long-lived task loop, which is meant to keep
> running.

---

## 6. Record schema (wire format)

One JSON object per session, a backward-compatible superset of today's
`HookRecord`. All fields except `updated` are optional; absent channels are simply
not set.

```jsonc
{
  "protocol": 1,                       // omitted ⇒ treat as v0 legacy record
  "state":    "running",               // §8 enum  (v0: the only field besides cwd/updated)
  "detail":   "running tests",         // optional qualifier for state
  "label":    "auth spike",            // §5.2 display name
  "topic":    "wiring the oauth callback", // §5.3 current focus
  "progress": { "value": 3, "total": 7, "detail": "file 3/7" },
  "attention":{ "reason": "permission: write outside cwd", "since": 1730000000000 },
  "fields":   { "git.branch": "feat/oauth", "tests.passed": "12/12" },
  "cwd":      "/home/u/.../worktree",  // the session's working directory
  "updated":  1730000000123            // unix millis of the last report (REQUIRED)
}
```

Field reference:

| Field | Type | Channel / §  | Notes |
|---|---|---|---|
| `protocol` | int | — | Schema version. Absent ⇒ legacy v0 record (state-only). |
| `state` | string | 5.1 / §8 | Activity state. |
| `detail` | string | 5.1 | Optional qualifier shown after state. |
| `label` | string | 5.2 | Display name; SHOULD be persisted by harness. |
| `topic` | string | 5.3 | Volatile current-focus text. |
| `progress` | object | 5.4 | `{value:int, total?:int, detail?:string}`. |
| `attention` | object | 5.5 | `{reason:string, since:int}`. |
| `fields` | object | 5.6 | string→string. |
| `events` | array | 5.7 | Optional bounded ring; harness MAY ignore. |
| `cwd` | string | — | Working directory; harness may use for grouping. |
| `updated` | int | — | **Required.** Unix millis; freshness/ordering key. |

---

## 7. Harness obligations

A conformant harness MUST:

1. **Observe liveness independently.** Determine whether each session's process is
   alive without consulting the report (in amux: `engine.Alive()` / tmux window
   presence). This is the authoritative "is it running" bit.
2. **Gate activity on liveness.** Combine per the function below. A dead process is
   `idle` even with a fresh `running` report; a live process with no record is
   `unknown`.

   ```
   resolve(alive, record):
     if not alive:                      return idle
     if record present and fresh:       return record.state   // running|waiting|ready|idle
     else:                              return unknown
   ```

3. **Age out stale records.** A record is *stale* if `now - updated > TTL` (default
   **300 s**, configurable). For a **live** session a stale record degrades to
   `unknown` (not trusted, but not dead). For a session with no liveness signal at
   all, staleness ⇒ treat as `idle`. Heartbeats (§5.8) and liveness both reset
   staleness.
4. **Join records to sessions by id**, and surface records whose id matches no
   known session as **untracked** rows (do not drop them).
5. **Project channels to the Snapshot.** Map `label → Session.Title`,
   `state → Session.State`, and compose a human `Session.Status` (e.g.
   `"waiting · permission"` from `state` + `attention.reason` or `topic`).
   Containers/roots inherit the most-demanding child state via the attention
   ladder (§8).
6. **Broadcast** the resulting `Snapshot` to subscribed UIs (one JSON object per
   line over the control socket). The harness is the only writer of `Snapshot`.

A harness MUST NOT require an agent to be reachable to report (file binding), and
MUST NOT parse agent output to derive any channel.

---

## 8. State enum & priority ladder

```
idle      no live process                                   (lowest attention)
ready     live; turn finished; ready for the next message
running   live; an active turn in progress
waiting   live; blocked awaiting human input
unknown   live; no fresh report yet
```

**Attention ladder** (how a parent/root row picks its state from children, and how
a UI prioritizes): `waiting > running > unknown > ready > idle`. A row showing
`waiting` always wins the user's eye over a sibling that is merely `running`.

Lifecycle → state mapping that runtimes SHOULD emit (matches the Claude binding):

| Lifecycle moment | Report |
|---|---|
| process started, no turn yet | `status ready` |
| a turn began | `status running` |
| blocked on the user (permission / idle prompt) | `status waiting` (or `attention <reason>`) |
| turn finished | `status ready` |
| process exiting | `status idle` |
| task complete (one-off agent) | `done` (§5.9) — terminal, archives the session |

---

## 9. Control plane (harness → agent)

The reverse direction reuses the existing daemon **action API** — newline-delimited
JSON `Action` requests, each answered by a `Result`, over the control socket
(`$AMUX_SOCK` / `$XDG_RUNTIME_DIR/amux.sock`).

Request envelope (`core.Action`) and response (`core.Result`):

```jsonc
// → request
{ "action": "rename", "id": "<session>", "fields": { "name": "auth spike" } }
// ← response
{ "type": "result", "ok": true, "newId": "" }
```

Actions relevant to an agent's lifecycle (existing): `open`/`attach`, `delete`/
`kill`, `move`, `archive`, `rename`, `new-repo-agent`, `add-agent`, `add-repo`,
`new-workgroup`, plus the `pane.*` streaming verbs (attach/detach a live terminal
without killing the agent). This spec adds `report` (§4c) as an action so a live
client can push channels over the same socket.

> The control plane and the reporting plane are deliberately separate: reporting is
> connectionless and unkillable (file binding), while control is connection-oriented
> (socket, request/response). `set_label` (§5.2) is the one channel that bridges
> them — a reported `label` is equivalent to a `rename` action and SHOULD be
> persisted the same way.

---

## 10. Conformance

**Agent (reporter) — MUST:** resolve identity per §3; exit `0` and never throw from
any report; write via atomic rename with read-modify-write merge (file binding) so
channels are independent; stamp `updated`. **SHOULD:** emit the §8 lifecycle
mapping; set `$AMUX_SESSION_ID`-derived identity when launched by the harness.

**Harness (consumer) — MUST:** observe liveness independently and gate per §7; age
out stale records; join by id and surface untracked; project channels to `Snapshot`
without parsing agent output; read both `reports/` and legacy `hooks/` (§11).
**SHOULD:** persist `label` durably; expose a TTL knob.

A minimal conformant agent implements only `report_status` (= today's behavior).
Everything else is additive.

---

## 11. Compatibility with v0 hooks

The shipped mechanism is exactly the `state`-only profile of this protocol:

- The `amux agent` namespace is in place. `amux agent hook <state>` is the
  Claude-settings binding (identity from stdin `session_id`, §3 rule 3);
  `amux agent status <state>` is the general verb; both write the same record.
- The pre-namespace top-level `amux hook <state>` and `amux name <text>` remain as
  deprecated aliases, so already-installed Claude settings keep working until the
  next `InstallHooks` run migrates them to the `amux agent hook` form.
- The legacy `HookRecord` (`{state, cwd, updated}`) is a valid v1 Record with
  `protocol` absent — readers MUST treat a missing `protocol` as v0 and a bare
  state-word file as `{state}` (the existing tolerant reader).
- Claude Code keeps writing via its installed `SessionStart/UserPromptSubmit/
  Notification/Stop/SessionEnd → amux agent hook <state>` hooks
  (`claudecfg.InstallHooks`). A v1 harness reading both directories sees these
  unchanged.

Migration is therefore non-breaking: the `amux agent` namespace ships first (done),
the merged reader and richer channels (`topic`/`progress`/`attention`/`fields`)
follow, and the top-level `amux hook`/`amux name` aliases stay until callers move
over.

---

## Appendix A — worked example

A test-runner agent, from launch to blocked-on-permission:

```sh
# harness launches it with AMUX_SESSION_ID=7f3a… in the env
amux agent status ready
amux agent label "ci triage"
amux agent status running
amux agent topic "bisecting the flaky auth test"
amux agent progress 2 --total 5 --detail "rerun 2/5"
amux agent field tests.failed 1
# needs to write outside the worktree:
amux agent attention "permission: write /etc/hosts"      # ⇒ state waiting
# user approves out-of-band; agent resumes:
amux agent attention --clear
amux agent status running
amux agent status ready
# the triage is finished and its fix is up for review — retire the session:
amux agent done                                          # ⇒ archived off the rail
```

Resulting record after the `attention` line:

```json
{
  "protocol": 1,
  "state": "waiting",
  "label": "ci triage",
  "topic": "bisecting the flaky auth test",
  "progress": { "value": 2, "total": 5, "detail": "rerun 2/5" },
  "attention": { "reason": "permission: write /etc/hosts", "since": 1730000000000 },
  "fields": { "tests.failed": "1" },
  "updated": 1730000000050
}
```

The rail renders `ci triage` with state `waiting · permission: write /etc/hosts`,
floated to the top by the attention ladder — provided the engine still reports the
process alive; if it died, the harness shows `idle` regardless.
