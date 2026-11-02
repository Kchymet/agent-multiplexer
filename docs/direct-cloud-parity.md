# Cloud relay → direct parity matrix

This is an audit of every operation the Harness cloud relay carries to a
machine, `amux provide` ↔ Harness. That surface is the `MuxMsg` types handled
in `internal/provider/provider.go` and the Harness routes that reach them. For
each operation it gives the direct (`amux direct`) equivalent, the permission
it needs, its endpoint, the evidence that it works, and any open gap. It was
written against master at c267381 plus PR #296.

**Evidence levels:**

- **live** means `internal/daemon/direct_e2e_test.go`. The real direct server
  drives a real daemon through `direct.HostBackend` over authenticated host
  connections: workspace source, store, wsops creation and steer dispatch.
  Only the agent runtime is a keystroke-recording fake.
- **unit** means direct-server tests against a fake backend.
- **daemon** means existing daemon tests of the shared dispatch path.

## Machine relay operations

The client path for every row is the shared web/Android shell over
`Authorization: Bearer`.

| Cloud relay operation (`harnessproto`) | Direct equivalent | Permission | Evidence | Status / gap |
| --- | --- | --- | --- | --- |
| `sessions-subscribe` → `sessions` snapshots | `GET /direct/v1/sessions`, `GET /direct/v1/sessions/watch` (WS) | read | S1 `TestCloudAbsentEndToEnd`; live | parity |
| `runtime-events-subscribe` → `runtime-events` | `GET /direct/v1/sessions/{sid}/stream` (SSE) | read | S1 e2e | parity |
| `runtime-events-history` → `runtime-events-page` | `GET /direct/v1/sessions/{sid}/history` | read | S1 e2e | parity |
| `session-action new-workgroup` (with first member when `repos` is set) | `POST /direct/v1/workgroups` | create, `all_sessions` | live: root plus first member, idempotent replay creates no duplicate | parity. `linear` is not accepted (put the link in `prompt`) |
| `session-action new-workgroup-empty` | `POST /direct/v1/workgroups` | create, `all_sessions` | live: `repos=""` (every tracked repo, live) and `repos=app` (exactly app) | parity |
| `session-action add-agent` | `POST …/{root}/actions` | create | live: starts the agent; a narrow ceiling refuses an outside repo before Apply; a scoped grant works inside and gets 404 outside | parity |
| `session-action rename` | actions | manage | live | parity |
| `session-action archive` / `unarchive` | actions | manage | live: `affected` lists the root and its members; inherited archive refuses prompt and member unarchive; root unarchive restores | parity |
| `session-action start` | actions | manage | live | parity |
| `session-action prompt` | actions | steer | live: keystrokes reach the runtime | parity |
| `session-action interject` | actions | steer (#295) | live: keystrokes | parity |
| `session-action stop` | actions | steer | live: idle stop refused (422), busy stop delivered | parity |
| `session-action goal` | actions | respond | live: Claude hook-goal coordinator paused with an objective (goal file changes); a budget gets the daemon refusal | parity (a Codex native goal is unit and daemon only) |
| `session-action permission` | actions | approve | unit, daemon (`runtimePermissionGate.consume`), runtimeevents binding tests | **live proof missing**: a supervised Codex runtime raising a correlated approval, with `request_id` and `runtime_generation` read back through direct history and the reply consumed. Only structured Codex sessions report `caps.permission`; Claude hook reports cannot be answered on either transport |
| Provisional follow-up on a just-created id (`sessions-new-workgroup-empty` feature) | Any route naming a session the same grant created | the verb's own | live `TestDirectLiveFollowUpOnNewIDBeforePoll` | **fixed in #296**: a miss on a self-created id waits up to 3 s for the daemon to publish it. All checks run on the published row |
| Harness/model/repo discovery (`register.capabilities.execution`; models and repos are not relayed) | `GET /direct/v1/capabilities` catalog | read (catalog with create) | unit; live repos and ceilings | direct gives more than cloud. Models are plain names only |
| `account-request status` (provider `--account-status`; `ManagedOnly` central profiles; ceremonies and mutations refused) | none | — | — | **gap, proposal below** |
| `account-ceremony` and the `default`/`login`/`logout`/`rebind` mutations | none | — | provider refuses them remotely | not a cloud function: the local CLI/TUI only |
| Account use at creation/start/prompt (provider `--account-use`, refused with `account_use_denied` when off) | Implicit in steer/manage/create | — | — | **gap, proposal below**: direct has no host switch |
| `spawn`/`input`/`resize`/`kill` panes (provider `--allow-compute`) | none | — | — | **Harness compute platform, not a machine-session workflow**: Harness runs its own pool workloads with it, and the cloud UI shows no terminal for local agents (`DaemonPane.tsx`: "no terminal is ever exposed for a local agent"). A direct PTY surface would be new scope and needs an explicit decision |
| `direct-enroll` | `amux direct enroll` / ticket claim | host | cloud fixtures | Harness brokers direct enrollment; nothing to mirror |
| `register`, `ping`, `pong`, `reset`, `provider-revoke` | `hello`, `token`, `revoke` | — | S1 | transport; equivalent |

## Harness-only platform features (not machine relay)

These are not served by the machine and have no direct counterpart by design:

- tenancy, sign-in (Google SSO), provider registration and sharing;
- cloud agents on GKE and compute pools (which use the `--allow-compute`
  panes);
- the backend catalog;
- FCM push for permission requests and turn ends. A direct client learns of
  these only while it is open (watch and stream). A push path that keeps
  cloud out of session traffic would be separate work.

## Proposals awaiting a decision

1. **Account status (read-only).**
   - Add a new explicit permission `accounts`, granted only when named at
     enrollment or in the cloud ceiling, never implied by `read`.
   - Serve it as `GET /direct/v1/accounts`: the central `ManagedOnly` profiles
     with allowlisted fields: `harness`, `state`, `loginMethod`, `mode`,
     `supportedModes`, `limits` (state, source, observed/fresh times,
     windows, `resetsAvailable`), and `identity.plan`.
   - `identity.email` and `identity.accountId` are personal identifiers.
     Include them only if the reviewer agrees; the cloud shows them today.
   - Never send credentials, ceremonies, `profileId` internals, session ids or
     runtime generations from limits.
   - Possibly also require the host's existing `provide --account-status`
     setting (the same consent the cloud uses).
2. **Account use.** Steer, manage-start and create already drive the
   machine's central account on direct. That matches the cloud default
   (`--account-use` is on when publishing read-write). For parity of the
   *off* switch, add a host setting `direct.account_use` (default on). When it
   is off, the server refuses `prompt`, `start` and the creation verbs with
   `account_use_denied`, as the relay does.
3. **Remote panes.** Out of scope unless the user wants a direct remote
   terminal. That would need its own permission (for example `terminal`) and
   its own review, since it is arbitrary command execution on the host.
