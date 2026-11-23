# Admin console, overseer, and direct access (design)

Status: revision 2. **Phase A (§6: the overseer role, the question queue, local
CLI/TUI) is implemented** — see [default-sessions.md](default-sessions.md),
[workgroup-permissions.md](workgroup-permissions.md#console-overseer-and-repo-homes)
and `amux inbox`. Phase B1 (direct exposure, §5) is implemented: see [direct-access.md](direct-access.md#question-queue-and-overseer) and the `queue-v1` fixtures. Phase C (step-up) is deferred.

Revision 2 splits today's console into two sessions:

- a **local-only admin console**;
- a new **overseer**, which supervises the coordinators and owns the single
  question queue.

Only the overseer and its queue are reachable over `amux device`. Anything
with admin power never is.

## 1. Why the console is excluded today

- **#273 (15fcf0e), direct S1.** It added `ConsoleID`, `isConsole`, and the
  early `return false` in `inScope` (`internal/direct/sessions.go`). It also
  added the `ValidateResource` refusal "the console cannot be granted over
  direct access". The reason given: the console "carries operator and account
  powers, so S1 direct access never exposes it … under any grant, including
  all_sessions".
- **#278 (96811de), cloud ceilings.** The ceiling is "read or steer,
  `--all-sessions` or `--workgroup`, with the console excluded".
  `withinCeiling` relies on "the console is never valid".
- **#296 (95ed43c), workflow verbs.** It added "console never in scope"
  before every `Apply`, and `actions.go` refuses the console as a creation
  parent.
- **#313 (2922c17), wake-ups.** `wake_service.go` and `wake_store.go` exclude
  the console.

**The root cause is laundering.** Direct fails closed for delete, move, repo
tracking, accounts and host administration. The console passes every
`Console` rule. A free-text prompt to the console would therefore turn into
exactly those operations, performed by an LLM.

Revision 2 fixes this structurally instead of gating it. Everything with
admin power stays local, and only a session without admin power is exposed.

## 2. The split

### Roles

| | Admin console | Overseer (new) | Coordinator |
| --- | --- | --- | --- |
| Session id / role | `console` / `console` | `overseer` / `overseer` | workgroup id / `coordinator` |
| Sandbox | `~/.local/share/amux/console/` | `~/.local/share/amux/overseer/` | `sessions/<wg>/coordinator/` |
| Job | amux administration: `doctor`, updates, auth/accounts, direct clients and enrollment, archive sweeps, cost and usage (`amux top`), repo tracking and grants | Supervises every coordinator, owns the cross-workgroup inbox, curates the one question queue | Supervises its own members |
| Reachable over direct | **Never** (permanent rule) | Yes, under the grants in §5 | Yes (ordinary session, as today) |
| Raises items to | — | the user (the queue) | the overseer (Part 1's "global inbox owner") |

**Why "overseer".** It oversees the supervisors.

- "Supervisor" collides with the coordinator's own `supervising` state
  (#319, #327).
- "Dispatch" collides with the coordinator's verb: coordinators *dispatch*
  members (`docs/default-sessions.md`).
- "Overseer" names the relationship without reusing either word.

### Lifecycle and guide

The overseer is a synthetic default session like the console: never a store
row, never deletable.

- **Starting.** The daemon starts it lazily, on first open or on the first
  escalation.
- **Waking.** It is woken by queue and inbox events, the same way #320 wakes
  a coordinator when a member's turn ends. Nothing polls.
- **Goal mode.** It runs in goal mode like coordinators (#180), so a queue
  backlog keeps it working. It never `goal ask`s itself; its questions are
  queue entries.
- **Guide.** Its guide is generated at every launch by a new `overseerGuide`
  in `internal/wsops/guide.go`. The guide carries the workgroup roster, the
  open queue, and the rules in §2.4. Durable notes go in its
  `.amux/claude/CLAUDE.md`.
- **Admin console guide.** The console's guide is narrowed to
  administration. It points the user to the overseer for workgroup work and
  no longer receives escalations.

### Access rules (`internal/access/scope.go`, `policy.go`)

These changes add a role, a marker and two predicates:

- `knownRole` gains `"overseer"`.
- `Rule` gains `Overseer bool` next to `Console bool`. `Console` is
  unchanged: the admin console keeps every power it has today.
- In `Authorize`, an overseer subject is admitted **only** for rules marked
  `Overseer: true`, whatever their scope. This keeps default deny:
  `ActionRename` and every other unmarked rule stay closed.
  - For `ScopeHost` rules, that marker is the whole check.
  - For `ScopeWorkgroup` rules, it also needs `InWorkgroupScope` and
    `allowOverseer(req, target)`, which replaces the coordinator-shaped
    `Allow` predicates.
  - There is no blanket shortcut like the console's.
- `InWorkgroupScope` gets a new case: `"overseer"` → `target.Role !=
  "console"`. So reads and events cover every workgroup, member and repo
  home, but never the console. `FilterSnapshot` inherits this.

| Rule | Today's marker | Overseer | Admin console |
| --- | --- | --- | --- |
| `QuerySessions/Snapshot/Repos`, `QueryRuntimeEvents`, `QueryWait` | filtered / workgroup | ✅ all but the console | ✅ |
| `ActionStart` | workgroup | ✅ coordinator rows only (starting a workgroup starts its members) | ✅ |
| `ActionSteer` prompt/interject/stop | workgroup | ✅ **coordinators only**; members are steered through their coordinator | ✅ |
| `ActionSteer` permission | workgroup | ❌ never (only the user answers, through the queue, §4) | ✅ |
| `ActionRename` | workgroup | ❌ | ✅ |
| `ActionSetArchived` | Console | ✅ `archived=true`, **no `force`**, target is a workgroup root (cascade) or a member; the open-PR guard always applies | ✅ |
| `ActionAddAgent` | Console | ❌ (asks the coordinator) | ✅ |
| `ActionNewWorkgroup` | Console | ✅ tracked repos only, and only with `from_item` naming an item the **user** acknowledged (§4) | ✅ |
| `ActionRefresh` | Console | ✅ (re-poll only) | ✅ |
| `ActionMove`, `Delete`, `Kill` | Console | ❌ | ✅ |
| `ActionAddRepo`, `RmRepo`, `AgentSetRepos` | Console | ❌ | ✅ |
| `ActionCreateWorkspace` | Console | ❌ (it uses `new-workgroup`) | ✅ |
| Archive toggle, restart, auth reload, account and codex login, coordinator set-repos, panes, raw runtime, accounts, resources | Host | ❌ | ❌ (unchanged) |
| New: `QueryQueue`, `ActionItemRaise`, `ActionItemUpdate` (§4) | — | ✅ (raise to the user, triage) | ✅ read-only |

No principal except the host and the admin console can steer the console.
Coordinators cannot steer the overseer either; they reach it only by raising
items. `TestEveryCoreOperationIsClassified` gains an overseer column, and a
new test pins every ❌ above.

### Needing an admin action

The overseer never escalates by proxy.

1. It raises a `decision_needed` item with `admin: true`. The item carries
   the exact local command (for example, "at the admin console: `amux repo
   add …`").
2. The queue shows the item with a "local admin" badge, and the only
   actions on it are acknowledge and dismiss.
3. The console is outside the overseer's scope, and items never flow to the
   console. The user does the action at the admin console.
4. The user marks the item done.

## 3. Threat model (revised)

**Ceiling.** Over direct, no reachable session can do anything outside what
an existing `all_sessions` grant with `steer,manage,create,approve` could
already do. That holds by construction (§5): the overseer has no admin power,
and steering it requires those permissions.

| Threat | What it now achieves | Control |
| --- | --- | --- |
| Stolen, unlocked phone | Whatever its grant allows: steer coordinators, answer questions, archive finished workgroups (never forced past the PR guard), create workgroups from acknowledged items. **No** accounts, auth, direct-client management, repo tracking, delete or move | Grant permissions; revoke; audit (§5) |
| Leaked refresh or access token | The same, until reuse detection or revoke | Unchanged from today |
| XSS in the Harness web client (bearer token readable by script) | The same as a leaked token | Unchanged from today; optional step-up below |
| Malicious web origin | Nothing | Exact `--allow-origin`, no credentialed CORS |
| Cloud relay compromise | Codes within the ceiling only; the console is never in any ceiling | Overseer steering only when the ceiling names its prerequisite permissions (§5) |
| Prompt injection via items or questions (repo or PR text → member → coordinator → overseer → phone) | A manipulated overseer can only steer coordinators, archive unforced, or create a workgroup **from an item the user acknowledged** | Daemon-enforced `from_item` check; no permission answers by the overseer; queue text rendered as plain text; https links only |
| Replay | Nothing | Answers bind question id + runtime generation + tool-use or request id; idempotency keys |

**Step-up is no longer required to steer the overseer.** Steering it grants
no authority the grant does not already hold, so the passkey design from
revision 1 drops out of the critical path.

What step-up still buys is resistance to theft of an unlocked device or a
token, for the two answers that act immediately and can't be taken back:

- answering a **permission request** (it executes a tool call in some
  session);
- **archiving a whole workgroup** (it stops every runtime in it).

That protection applies equally to today's `approve` and `manage` verbs on
any session, so it is a general direct hardening, not a console feature. It
is scheduled as an optional phase C (§6), off for existing grants and offered
at enrollment.

## 4. The question queue

**Model.** There is one queue view over two record types that share an
envelope `{id, kind, source_session, workgroup, title, body, options,
created_at, state, answer_route}`:

- **Live questions** are derived by the daemon from runtime state. Nobody
  writes them, and they disappear once answered or stale.
- **Items** (Part 1) are durable and raised explicitly. `decision_needed`
  items appear in the queue; the other kinds appear only in the inbox.

The two are kept separate because their lifetimes differ. A live question is
valid only while its runtime is blocked; an item survives restarts.

| Kind | Source signal (already on master) | `id` (dedupe key) | Answer route |
| --- | --- | --- | --- |
| `question` (Claude `AskUserQuestion`) | #327 `PreToolUse` hook → waiting; options from the transcript tail via `claudeQuestionOpen` (#316) | hash(session, runtime generation, tool_use_id) | `steer prompt` with the text formatted per harness-android#73 `questionReply`. The daemon presses Esc on the selector first (#316) |
| `permission` (Claude and Codex) | runtime `permission_request` event | hash(session, generation, request_id) | `steer permission` with request_id + runtime_generation + allow/deny. The gate consumes it once; there is no "always" |
| `goal_input` | `amux agent goal ask` (`claudegoal.StatusInput` + its question) | hash(session, goal id, ask sequence) | `steer prompt`; the goal prompt hook resumes the goal |
| `decision_needed` | Part 1 item | item id | The answer is recorded on the item, delivered as a prompt to the source session, and the item is marked done |

**Who answers.** The user does: at the host (`amux inbox`, the TUI queue
pane) or through a direct client. The daemon delivers the answer on the
answerer's own authority and records who answered.

The overseer **curates** the queue but never answers in the user's place.
Curating means it may:

- set a `suggested_answer` the user confirms with one tap;
- link duplicates;
- add context.

It never answers permission requests. Answering AskUserQuestions or goal
asks for the user is out too, because that would launder the user's
decision.

**Dedupe.**

- The deterministic id merges the same question seen through several signals
  (for example, Notification plus `PreToolUse`).
- A coordinator whose member is blocked must not `goal ask` the same thing.
  The coordinator guide says so, and the queue already shows the member's
  question under its workgroup. When it does anyway, the overseer links the
  two, and answering one resolves both.

**When the asker has gone away.** Before delivery the daemon re-checks the
entry against fresh state (same generation; question still open with that
tool_use_id; request still pending).

- An `AskUserQuestion` or a permission whose turn ended, whose runtime
  restarted, or whose session was archived becomes `stale`. The daemon
  answers `409 asker_gone`.
- From a stale entry, the client offers "send as a new message", an explicit
  `steer prompt` the user confirms.
- A `goal_input` answer survives a stopped coordinator: the prompt
  cold-starts it (#86).

**Related work, integrated rather than duplicated.** I found the "questions
don't propagate" work on master as #316 (dismiss + typed reply), #327
(`PreToolUse` question hook, `Supervision.NeedsYou`) and harness-android#73
(the question card). There is no open PR for it. The queue reuses those
signals and that answer path. The only new step is lifting them out of each
transcript into one place.

Phone wake-ups already fire on `permission_request` and turn-end (#313). A
content-free wake for `question` and `goal_input` is a later addition to that
contract.

## 5. Direct exposure (Part 3)

**No new permission names.** Overseer access is derived from existing
permissions. Steering the overseer needs exactly the permissions for
everything the overseer can do, so it never launders.

| Capability | Requires |
| --- | --- |
| Queue and inbox read: `GET /direct/v1/queue`, `/queue/watch` (WS), `/summary` (Part 2 buckets) | `read`. An `all_sessions` grant sees everything. A workgroup grant sees entries whose source is in scope (its own workgroup's questions) |
| Answer a `question` / `goal_input` / `decision_needed` (`POST /queue/{id}/answer`) | `steer` on the source session (in scope) |
| Answer a `permission` | `approve` on the source session |
| Item triage (acknowledge, dismiss, done, reopen) | `steer` in scope. An acknowledgment counts as "user acknowledged" for `from_item` only from a grant holding `create` + `all_sessions` |
| Overseer row in inventory, its history and stream | `read` + `all_sessions`, and the client opts in with `?include=overseer` |
| Steer the overseer (prompt, interject, stop) | `all_sessions` + `steer` + `manage` + `create`. `approve` is not needed, because the overseer never answers permissions |
| Admin console | Never: `inScope`, `ValidateResource`, the cloud ceilings, the wake service and `actions.go` keep the exclusion for `console`, permanently |

- **Answer request.** `{answer | choice[], decision: allow|deny,
  runtime_generation, idem_key}`, with an exact field allowlist per kind.
  - The check order is #296's, plus a fresh stale check before Apply.
  - Errors: 409 `asker_gone`, 409 `answered` (someone else answered first).
- **Cloud ceilings.** Nothing new. Overseer steering is reachable through
  cloud enrollment only when the ceiling already names `steer,manage,create`
  with `--all-sessions`. The console remains invalid in every ceiling.
- **Audit.** `direct-audit.jsonl` in the direct state dir (0600, rotated).
  - It records every queue answer, item triage and overseer steer over
    direct: time, grant id and label, verb, entry or item id, text capped at
    4 KiB, outcome.
  - The entry is written before the response, and a failed write fails
    closed.
  - `amux device audit [--client] [--json]` reads it. Delivered answers
    carry `via: direct` provenance in event metadata.
- **Revocation.** The existing `amux device revoke`, unchanged.
- **Backward compatibility.**
  - `capabilities.queue {read, answer_kinds}` and `capabilities.overseer
    {steer}`. When absent, the host predates them.
  - Old hosts 404 the new routes.
  - Old clients never send `include=overseer`, so their inventory is
    unchanged.
  - No permission names are added, so there is no store or ceiling
    migration.
  - Fixtures go in `docs/direct/fixtures/queue-v1` (additive).

**Clients.**

- **harness web** (`frontend/src/direct/`, `components/rail/`).
  - The existing `NeedsYouPane.tsx` becomes the queue. Entries are grouped by
    workgroup, with a kind chip and plain-text body.
  - Options render as buttons, generalizing the android #73 card. Permission
    entries get allow and deny buttons.
  - Stale entries are greyed, with "send as new message".
  - There is a separate inbox tab for the other item kinds, with triage
    actions, and a summary header with the Part 2 buckets.
  - The overseer gets a chat view that reuses `useDirectTranscript` and
    `useDirectSteering`, shown only when `capabilities.overseer.steer`.
  - Tests: vitest against `testing/fakeServer.ts`, fixture conformance, and
    a `ci/e2e/direct` driver.
- **harness-android.**
  - `core/network/.../direct/DirectApi.kt` and `DirectWire.kt` get the queue
    routes.
  - A `QueueScreen` reuses the `QuestionCard` and sits beside the session
    list.
  - The overseer chat uses the existing steered-session route.
  - Tests: JVM fixture tests (the `DirectFixturesTest` pattern) and
    ViewModel tests for stale and answered races.

## 6. Implementation plan (one PR per repo per phase)

Part 1 (0ff685) keeps the escalation target abstract (a "global inbox
owner"). Part 2 points it at the overseer.

**Phase A: Part 2, agent-multiplexer (local only).**

- **Overseer role.**
  - `knownRole`, the `Rule.Overseer` marker, `allowOverseer`, and the
    `InWorkgroupScope` case.
  - The daemon check on `new-workgroup` `from_item`.
- **Session lifecycle.**
  - A synthetic default session, lazily started and woken by queue and inbox
    events.
  - `overseerGuide`; the narrowed console guide; the global inbox owner set
    to `overseer`.
- **Queue.**
  - A daemon queue derived from the signals in §4.
  - `QueryQueue`, `ActionQueueAnswer` (stale and answered checks),
    `ActionItemRaise`, `ActionItemUpdate`.
- **Local interfaces.** `amux inbox [list|answer|dismiss]` and the TUI queue
  pane.
- **Docs.** Update `default-sessions.md` and `workgroup-permissions.md`.
- **Tests.**
  - The access matrix (every ✅ and ❌ in §2).
  - The overseer can never reach the console, and coordinators can never
    steer the overseer.
  - Queue derivation and dedupe for each kind.
  - Stale on generation change.
  - Double-answer race.
  - Permission answers refused for the overseer.
  - `from_item` requires a user acknowledgment.

**Phase B: Part 3, direct exposure. Stacked on Phase A.**

1. **agent-multiplexer.**
   - Queue, summary and watch routes; the answer route; item triage.
   - `include=overseer` with derived steer gating.
   - Audit plus `amux device audit`; capabilities; `queue-v1` fixtures.
   - A permanent-exclusion test suite for the console.
   - Doc updates.
   - Tests:
     - every existing grant shape gets exactly the derived access;
     - workgroup grants see only their own entries;
     - zero backend calls on denial;
     - audit fails closed;
     - old-client inventory is byte-identical;
     - an e2e run against a live daemon.
2. **harness:** the queue pane, inbox, summary and overseer chat (after B1).
3. **harness-android:** the queue screen and overseer chat (after B1).

**Phase C (optional, later): step-up hardening for `permission` answers and
whole-workgroup archive.**

- **agent-multiplexer:** step-up key at redemption, challenge and assertion,
  and a per-grant `require_step_up` set at enrollment (default off for
  existing grants).
- **harness:** WebAuthn passkey.
- **harness-android:** a Keystore key behind BiometricPrompt.
