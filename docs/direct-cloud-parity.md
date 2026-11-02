# Cloud relay → direct parity matrix

This audits every operation the Harness cloud relay carries to a machine
(`amux provide` ↔ Harness): the `MuxMsg` types handled in
`internal/provider/provider.go`, plus the Harness routes and client payloads
that reach them. For each it gives the direct (`amux direct`) equivalent, its
permission and endpoint, the evidence, and what is still open. It was written
against master 7fe4ed9 plus PR #299.

**This is backend and protocol evidence only.** The web and Android direct
clients are still being built by their owners. Nothing here claims that a
user can already do these things from a direct client UI.

## Evidence levels

- **live:** `internal/daemon/direct_e2e_test.go`. The real direct HTTP server
  drives a real in-process daemon through `direct.HostBackend` over
  authenticated host connections: workspace source, store, wsops creation,
  steer dispatch and the permission gate. The agent runtime is a fake that
  records keystrokes. For approvals, a real `codexapp.Supervisor` talks to a
  fake App Server.
- **unit:** direct-server tests against a fake backend.
- **daemon:** existing daemon tests of the shared dispatch path.

## Machine relay operations

| Cloud relay operation (`harnessproto`) | Direct equivalent | Permission | Evidence | Status |
| --- | --- | --- | --- | --- |
| `sessions-subscribe` → `sessions` snapshots | `GET /direct/v1/sessions`, `GET /direct/v1/sessions/watch` (WS) | read | S1 `TestCloudAbsentEndToEnd`; live | backend parity |
| `runtime-events-subscribe` → `runtime-events` | `GET /direct/v1/sessions/{sid}/stream` (SSE) | read | S1 e2e | backend parity |
| `runtime-events-history` → `runtime-events-page` | `GET /direct/v1/sessions/{sid}/history` | read | S1 e2e; live (approval payloads) | backend parity |
| `session-action new-workgroup`, which creates a first member when `repos` is set and takes `linear` | `POST /direct/v1/workgroups` | create, `all_sessions` | live (below) | backend parity (#296, #299) |
| `session-action new-workgroup-empty` | `POST /direct/v1/workgroups` | create, `all_sessions` | live (below) | backend parity (#296, #299) |
| `session-action add-agent` | `POST …/{root}/actions` | create | live (below) | backend parity |
| `session-action rename` | actions | manage | live | backend parity |
| `session-action archive` / `unarchive` | actions | manage | live (below) | backend parity |
| `session-action start` | actions | manage | live | backend parity |
| `session-action prompt` | actions | steer | live: keystrokes reach the runtime | backend parity |
| `session-action interject` | actions | steer (since #295) | live: keystrokes reach the runtime | backend parity |
| `session-action stop` | actions | steer | live: idle stop refused (422), busy stop delivered | backend parity |
| `session-action goal` | actions | respond | live (below) | backend parity. A Codex native goal is unit/daemon evidence only |
| `session-action permission` | actions | approve | live (below) | backend parity after the #299 fix |
| Acting on a just-created id before the next inventory | Any route naming a session the same grant created within the last minute | the verb's own | live `TestDirectLiveFollowUpOnNewIDBeforePoll` | **fixed in #299** (see below) |
| Machine creation status (`daemon.create_status`: queued → preparing → ready \| failed) | `"async": true` on a creation, then `GET /direct/v1/creations/{creation_id}` | create | unit (acceptance matrix, fixtures); live `TestDirectLiveAsyncSlowCreation`, `TestDirectLiveAsyncRevokeAfterSendIsUncertain` | backend parity; outcome past the 10-minute bound or a restart after dispatch stays `uncertain` (see below) |
| Harness/model/repo discovery | `GET /direct/v1/capabilities` catalog | read (the catalog needs create) | unit; live repos and ceilings | backend parity, and direct is wider: the relay advertises harnesses only. Models are plain names only |
| `account-request` with `status` | `GET /direct/v1/accounts` | accounts, `all_sessions` | unit (gate on route, capabilities, list, watch, watch update; projection; attribution matrix); live `TestDirectLiveAccounts` | backend parity |
| `account-ceremony`, `default`, `login`, `logout`, `rebind` | none | — | `provider/accounts.go` refuses all but status remotely | not a cloud function: local CLI/TUI only |
| `spawn`/`input`/`resize`/`kill` panes (provider `--allow-compute`) | none | — | — | not a machine-session workflow (see below) |
| `direct-enroll` | `amux direct enroll`, ticket claim | host | cloud fixtures | Harness brokers direct enrollment |
| `register`, `ping`, `pong`, `reset`, `provider-revoke` | `hello`, `token`, `revoke` | — | S1 | transport equivalent |
| Session-event push (FCM on `permission_request` and watched `turn_end`) | none | — | — | **gap**: design pending the user's decision (see below) |

### Live evidence details

**Root creation (`new-workgroup`):**

- A root with a first member, and its idempotent replay, which creates no
  duplicate.
- The cloud UI's exact payload works.
- `linear` is passed through like the relay. The daemon prepends
  `Linear issue to work on: <link>` to the coordinator's task, and the live
  test reads the result from the store.

**Empty root (`new-workgroup-empty`):**

- `repos=""` means every tracked repo, live, as on the daemon.
- `repos=app` means exactly `app`.
- The cloud UI's exact payload (`prompt:""`, `agent`, `model`,
  `identity_mode:machine`) is accepted and creates no member.

**`add-agent`:**

- The agent starts.
- The cloud UI payloads are accepted: `repos:"api", prompt:""`, and the
  repo-less `repos:"", prompt:""`, which creates an agent with no
  repositories.
- A narrow ceiling refuses an outside repo before Apply.
- A workgroup-scoped grant works inside its workgroup, gets 404 outside it
  and 403 for root creation.

**`archive` / `unarchive`:**

- `affected` lists the root and its members.
- Inherited archive refuses a prompt to a member, and refuses unarchiving
  that member on its own.
- Unarchiving the root restores it.

**`goal`:** a Claude hook-goal coordinator is paused with an objective, and
the goal file changes. A token budget gets the daemon's own refusal.

**`permission`:** a supervised Codex runtime raises a command approval.

- Direct history carries `request_id` and `runtime_generation`.
- A forged generation is refused before the runtime.
- The exact pair delivers `{"decision":"decline"}` to the runtime once.
- A second answer is refused.

### Fixed in #299: structured approvals could not be answered

**Cause.** `runtimeevents.applyPending` decoded a request's `options` as
`[]string`. The structured supervisor publishes them as objects, so the decode
failed and `OpenPermissions` dropped the request. The daemon then never bound
a `runtime_generation` to it and refused every answer. This affected both
cloud and direct.

### Fixed in #299: acting on a just-created id

**Cause.** Direct authorizes from the daemon's cached rail, so a request made
right after creation could get 404. The cloud relay handles this with
provisional ids.

**Fix.** Direct re-reads the inventory for up to 3 seconds, and only for an id
the same grant created within the last minute. Every check still runs on the
published row.

**Resolved: replay identity.** A replayed creation used to answer with its
recorded `new_id` even after that session was deleted and its id reused. Now
the server records the created session's and root's store incarnations and
checks them on every replay (sync and async POST, status GET): a deleted,
reused or replaced target answers `created_target_unavailable` without ids, a
record from before incarnations `created_identity_unverifiable`, and a
transient failure to check `503 created_identity_unavailable` with the record
unchanged. The 3-second wait for a just-created id that is not yet in the
inventory starts only if the id's current incarnation is the created one,
and a row that appears during it is used only if that still holds. The cloud relay's provisional ids are
a different mechanism; direct needs no Harness change for this. Creating over
direct now needs a daemon with creation identities: an older daemon answers
`503 daemon_upgrade_required` before anything is sent, and capabilities say so
([Replaying a creation](direct-access.md#replaying-a-creation)).

### Machine creation status

Harness wraps machine creation asynchronously, because fetching repos and
creating worktrees can be slow. Direct creation is synchronous under the
60-second execution bound by default; a client that sends `"async": true`
gets the same queued → preparing → ready | failed flow from
`GET /direct/v1/creations/{creation_id}`, with no Harness traffic
([direct-async-create.md](direct-async-create.md)). Remaining limit: a creation
that outlives the 10-minute bound, or whose server restarted after sending, is
reported `uncertain` (it may exist) because the daemon has no creation request
id to reconcile it.

### Panes: not a machine-session workflow

Harness uses the relay's `--allow-compute` panes for its own pool workloads.
The cloud UI exposes no terminal for machine sessions (`DaemonPane.tsx`: "no
terminal is ever exposed for a local agent"). Creation plus the transcript
streams is the direct equivalent. No raw PTY surface is planned.

## Accounts

**Permission.** A new explicit permission `accounts`, implied by no other
permission. Machine-wide account metadata requires `accounts` and an
`all_sessions` grant, matching the cloud's owner-only authority. Issuing such
a grant, or a cloud ceiling that includes it, is the host's consent. The
relay's `--account-status` and `--account-use` flags stay relay-only. Direct
adds no account-use switch: the explicit steer, manage and create
permissions are the consent to use the machine's accounts.

**Route.** `GET /direct/v1/accounts` returns the daemon's `status` snapshot,
`ManagedOnly`, with the ceremony always cleared. It carries no mutation,
revision/CAS, auth URL, attempt or token fields.

**Capabilities.** An `account_status` boolean, always serialized:

| Value | Meaning |
| --- | --- |
| absent | The server predates accounts |
| `false` | Supported, but this grant or its scope may not see accounts |
| `true` | Allowed |

**Inventory rows.** A row gets `account_binding` (`profile_id`, `source`) only
when the grant holds `accounts` with `all_sessions`. One helper decides this
for the route, the capability flag, the list and every watch snapshot.

**Vocabulary.** Harness's broker accepts the seventh permission name in its
own follow-up PR. The full wire contract is in
[direct-access.md](direct-access.md#machine-accounts).

## Harness-only platform features (not machine relay)

- tenancy, sign-in (Google SSO), provider registration and sharing;
- cloud agents on GKE and compute pools;
- the backend catalog;

## Gap: notifications (a machine-workflow gap, design pending)

This is not Harness-only scope. On the cloud, the machine's own session
events drive FCM pushes: the provider's runtime-event ingest emits
`permission_request` and watched `turn_end`, and the Android actions reply
through Harness. A direct client learns of these events only while it is
open (watch and stream). There is no direct push path.

One option has been put to the user: an optional, content-free Harness
wake-up, with the session fetch and the approval reply staying direct. Until
the user chooses, nothing is implemented.
