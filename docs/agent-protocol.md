# amux Agent Protocol (AAP) v0

A common protocol by which an **agent** reports its state, identity, and progress
to an **orchestration harness** (the amux daemon), and by which the harness drives
the agent. It generalizes the existing Claude-hook status mechanism into an
agent-agnostic, multi-channel contract that any runtime (Claude Code, a shell
script, a custom agent) can implement.

- **Status:** draft / v0.
- **Compatibility:** the current `amux hook <state>` behavior is a strict subset
  of this spec (see [§11 Compatibility](#11-compatibility-with-v0-hooks)). Legacy
  generated commands stay nondisruptive, but isolated sessions need the fixed
  access context supplied at launch before reports have an effect.
- **Baseline implementation today:** `internal/core/hookstate.go`,
  `internal/claudecfg/claudecfg.go`, `cmd/amux/agent.go`,
  `internal/source/workspace.go`, `internal/daemon`.

---

## 1. Design principles

1. **Agents push; the harness never scrapes.** Activity is reported explicitly by
   the agent. No transcript/PTY parsing is used to *infer* meaning. (The harness
   independently observes process *liveness*; see §7.)
2. **Generated reporting must never disrupt the agent.** Hook/status-line forms
   swallow errors and exit `0`; explicit diagnostic commands preserve errors.
   Both use bounded authenticated RPC and never regain direct shared-state access.
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
| **Session subject/runtime** | The daemon-authoritative amux subject plus its current stored runtime identity. Neither component is selected by report input. |
| **Rail** | A UI that renders the harness `Snapshot`. A pure consumer; never a party to this protocol. |
| **Record** | The current reported state for one session (§6). |

---

## 3. Identity & addressing

Every managed report uses the fixed read-only session access context mounted by
the harness. Its signed credential identifies exactly one amux subject. The
daemon resolves that current store row and current live runtime generation, then
derives the record target from the authoritative subject/runtime pair. Explicit
IDs, `$AMUX_SESSION_ID`, hook `session_id`, cwd, transcript paths, tmux metadata,
and report-supplied UUIDs are never identity or storage authority.

Generated telemetry hooks are silent on missing/invalid context so observability
cannot interrupt the runtime; explicit report commands return an error. Legacy
UUID-only files and untracked host diagnostics remain preserved, but are outside
the managed self-report authorization path.

---

## 4. Transport bindings

Managed self-report has one authority-bearing binding: signed regular-file RPC
through the fixed session access context. Legacy files remain diagnostics, not
an interchangeable write path.

### 4a. CLI binding (normative, primary)

The agent invokes the `amux` binary as a subprocess:

```
amux agent <verb> [args...]
```

The CLI opens only the fixed context, queries the daemon's current opaque runtime
generation, and submits a signed bounded report. Generated hooks exit `0` after
one best-effort attempt; explicit invocations report errors.

### 4b. Legacy files (diagnostic only)

v0 UUID-only files under `hooks/`, `models/`, `permissions/`, and transcript
backups are preserved for host diagnostics. Namespace-isolated sessions do not
see those roots, and direct file writes are not a managed self-report binding.
Daemon-owned managed records are keyed by authoritative subject plus runtime.

### 4c. Host control socket

The ordinary daemon socket is a trusted host management surface, not a fallback
for isolated self-report. A session report never dials it, starts a daemon, or
uses caller-supplied `id` to select a record.

---

## 5. The reporting API (agent → harness)

Language-neutral function surface. Generated telemetry bindings are
**best-effort**; explicit CLI diagnostics preserve failures. The durable
`report_done` control returns failure unless archive is confirmed (§5.9).
Only state reporting, label/name, model/capture/permission diagnostics, and
`done` are shipped today; the other channels below remain additive proposals and
do not authorize a generic caller-defined report map.

### 5.1 `report_status(state, detail?)`

Set the agent's lifecycle/activity state.

- **CLI:** `amux agent status <state>`
- **Channel:** `state`
- **`state` ∈** the enum in §8. Unknown values are rejected.
- A richer `detail` qualifier remains a future additive channel.
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
- **Transport:** a future implementation must use a bounded authenticated report
  operation. The host management socket and direct legacy files are not fallbacks.
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
- **Identity:** unlike the activity verbs, `done` acts on the authenticated
  principal's *store* subject (the id archive/rename actions take). It accepts no
  `--id` or environment identity. Missing fixed context fails nonzero.
- **Bridges the two planes (like §5.2 `set_label`).** `done` is reported through
  the control plane (the `set-archived` action, §9) because archival is durable
  store state the harness owns, not a volatile activity channel. It exits `0`
  only after the daemon confirms the archive action; missing identity, transport
  denial, an unreachable daemon, and rejected actions are nonzero failures.
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
`HookRecord`. Managed records additionally bind `subject_id` and `runtime_id` to
their daemon-derived storage tuple. All fields except `updated` are optional;
absent channels are simply not set.

```jsonc
{
  "protocol": 1,                       // omitted ⇒ treat as v0 legacy record
  "subject_id":"agent-a",             // daemon-derived for managed records
  "runtime_id":"conversation-1",      // daemon-derived for managed records
  "state":    "running",               // §8 enum  (v0: the only field besides cwd/updated)
  "detail":   "running tests",         // optional qualifier for state
  "label":    "auth spike",            // §5.2 display name
  "topic":    "wiring the oauth callback", // §5.3 current focus
  "progress": { "value": 3, "total": 7, "detail": "file 3/7" },
  "attention":{ "reason": "permission: write outside cwd", "since": 1730000000000 },
  "fields":   { "git.branch": "feat/oauth", "tests.passed": "12/12" },
  "cwd":      "/home/u/.../worktree",  // daemon-derived session directory
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
| `subject_id` | string | — | Authoritative managed subject; never report-selected. |
| `runtime_id` | string | — | Authoritative stored runtime identity; never report-selected. |
| `cwd` | string | — | Daemon-derived working directory; harness may use for grouping. |
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
4. **Join managed records by subject/runtime**, and surface preserved legacy
   UUID-only records separately as **untracked** diagnostics (do not let them
   override a managed row).
5. **Project channels to the Snapshot.** Map `label → Session.Title`,
   `state → Session.State`, and compose a human `Session.Status` (e.g.
   `"waiting · permission"` from `state` + `attention.reason` or `topic`).
   Containers/roots inherit the most-demanding child state via the attention
   ladder (§8).
6. **Broadcast** the resulting `Snapshot` to subscribed UIs (one JSON object per
   line over the control socket). The harness is the only writer of `Snapshot`.

A harness MUST NOT parse agent output to derive a channel. It may independently
observe liveness but applies report effects only after authenticated admission.

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
without killing the agent). Self-report verbs are a separate closed session-RPC
namespace and are not added to this host action vocabulary.

> The control plane and reporting plane are deliberately separate. Both are
> request/response, but session RPC authenticates one fixed subject and admits only
> its bounded vocabulary; the host socket retains broader management authority.

---

## 10. Conformance

**Agent (reporter) — MUST:** use only the fixed authenticated context in §3 and
never select a record path or subject; generated hooks exit `0`, while explicit
commands preserve errors. **SHOULD:** emit the §8 lifecycle mapping.

**Harness (consumer) — MUST:** observe liveness independently and gate per §7; age
out stale records; join managed records by authoritative subject/runtime; project
channels to `Snapshot` without parsing agent output. Legacy UUID-only records may
be surfaced separately as untracked host diagnostics but never override managed
state.
**SHOULD:** persist `label` durably; expose a TTL knob.

A minimal conformant agent implements only `report_status` (= today's behavior).
Everything else is additive.

---

## 11. Compatibility with v0 hooks

The shipped mechanism is exactly the `state`-only profile of this protocol:

- The `amux agent` namespace is in place. `amux agent hook <state>` is the
  nondisruptive Claude-settings binding; `amux agent status <state>` is the
  explicit error-reporting form. Both use the fixed authenticated session
  context. Stdin `session_id`, environment IDs, and paths are ignored for
  authority and storage selection.
- The pre-namespace top-level `amux hook <state>` and `amux name <text>` remain as
  deprecated aliases, so already-installed Claude settings keep working until the
  next `InstallHooks` run migrates them to the `amux agent hook` form.
- The legacy `HookRecord` (`{state, cwd, updated}`) is a valid v1 Record with
  `protocol` absent — readers MUST treat a missing `protocol` as v0 and a bare
  state-word file as `{state}` (the existing tolerant reader).
- Claude Code keeps reporting via its installed `SessionStart/UserPromptSubmit/
  Notification/Stop/SessionEnd → amux agent hook <state>` hooks
  (`claudecfg.InstallHooks`). Generated commands stay nondisruptive, while their
  effect now requires the fixed authenticated session context.

Legacy UUID-only files remain preserved as host diagnostics, but managed readers
use daemon-owned subject/runtime-scoped records. A new binary in an old namespace
cannot restore the hidden shared-state mount: explicit reports fail and generated
hooks exit zero until the session is relaunched with fixed session access.

Migration is therefore fail closed: the `amux agent` namespace ships first (done),
the merged reader and richer channels (`topic`/`progress`/`attention`/`fields`)
follow, and the top-level `amux hook`/`amux name` aliases stay until callers move
over.

---

## Appendix A — worked example

A test-runner agent, from launch to blocked-on-permission:

```sh
# harness launches it with a fixed authenticated session access context
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
