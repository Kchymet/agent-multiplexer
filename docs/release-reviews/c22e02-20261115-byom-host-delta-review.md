# BYOM host delta review: 2d32526 → cb6b7b2

Report only. No product code changed. Reviewer: Claude Code on
`claude-opus-5-5` (Opus 5.5, confirmed from the runtime environment). This is
a source review plus isolated component tests. It is **not** runtime, device or
live-service proof.

## Range

- The accepted host `2d3252658f064f0bff352b2db1a30a47e4b2670b` is the tip of PR
  branch `amux/c22e02-ec2ee8`, not a master commit. Its merge base with master
  is `c0e06c5` (#276). Its patch equals master's squash `95b109b` (#282),
  checked with `git diff c0e06c5 2d32526` against `git diff 0543932 95b109b`.
- Master landed #275 and #277–#280 *before* #282, so none of them are in
  2d32526. The reviewed delta is the tree diff `2d32526..cb6b7b2`: #275 and
  #277–#305 except #282.
- Whole-tree size of `git diff --stat 2d32526 cb6b7b2`: **325 files,
  +31462/−622**.
  - Correction: the first version of this report said "140 files,
    +6696/−123". That figure was a measurement error, not a scope filter. It is
    `2d32526..95b109b` (master only up to #282).
  - The non-test, non-doc, non-fixture source outside the opt-in direct trees
    (`internal/direct`, `internal/directsvc`, `internal/tailnet`,
    `cmd/amux/direct*`) is 78 files, +5888/−330. The review focused on the
    BYOM-relevant part of that.
- Master `cb6b7b281bd9c6d3880c695054b7dd1ddc51f199` CI run `41570536223` is
  green.

Out of scope as opt-in, except where noted: `internal/direct`, `directsvc`,
`tailnet`, `cmd/amux/direct*`, procstat/top/nativetui, and CI scope scripts.
The Harness-provider CI drift belongs to worker c60743.

**This is not a complete review of the optional direct code.** Direct code was
checked only at the points where it touches the normal cloud-brokered path:

- the provider's direct-enroll gate;
- the daemon creation-identity entry point;
- the supervisor's default-off state.

The rest of direct (server, authority, enrollment, tailnet, async creation) was
not reviewed for correctness.

## Verdict

**No concrete new blocker found in source** for the normal cloud-brokered BYOM
path. **cb6b7b2 can replace old2d as the host for a bounded provider
regression**, and is the better choice. Reasons:

1. **The default wire is unchanged.** The register message gains only optional
   fields. `Capabilities.Direct` and the `direct-enroll-v1` feature are sent
   only when `--direct-enroll` is on *and* the direct server reports issuance.
   Opaque config strings can never inject that feature
   (`internal/provider/provider.go:493`, `cmd/amux/provide.go:276`). A broker
   that sends `direct-enroll` anyway gets `disabled`
   (`internal/provider/directenroll.go:190`). `MRegistered.Owner` is optional
   and display/pin-default only. It is reset on every registration
   (`internal/provider/provider.go:374`), and it authorizes nothing.
2. **old2d has a relay bug that cb6b7b2 fixes (#299).** At 2d32526,
   `steerFields` dropped `runtime_generation`. The daemon gate rejects an empty
   generation (`internal/daemon/session_permission.go:299`). So every
   cloud-relayed permission decision failed on old2d. cb6b7b2 forwards the
   field (`internal/provider/sessions.go:550`), and a test pins it
   (`internal/provider/sessions_test.go:494`).
   - **Service side (evidence from the coordinator, not inspected here):** the
     root reports that in service `fe48`, `actions/providers.go:268` copies
     `runtime_generation` and `:295` requires it. The root also independently
     confirmed that old2d drops the field and cb6b7b2 forwards it. So the
     current host fixes a real defect in the old candidate.
   - End to end over the live service it is still unverified (G2).
3. **Store changes are additive inside schema v3,** so the old binary still
   opens the store. Downgrade caveat F2 below applies.

## Scope findings

### Provider registration, self-revocation, owner echo (#286 #289 #290 #291)
- `amux provide revoke --yes` (`cmd/amux/provide_revoke.go:19`):
  - It is host-only through fixed-path session-context provenance, which fails
    closed (`cmd/amux/main.go:237`).
  - It requires confirmation before reading any host state.
  - It sends a standalone `provider-revoke` first frame and never registers.
  - It reads at most 16 KiB and never echoes peer text
    (`internal/provider/revoke.go:22`).
  - Direct-client grants survive by design.
- One provider per host: an flock on `provider-status.json.lock`
  (`internal/provider/statuslock.go:24`, wired at `cmd/amux/provide.go:282`).
  A second provider fails safe.
- Harness discovery: a missing optional harness is now "skipped", not a
  failure. The advertisement is unchanged.

### Authenticated daemon/session access and grants
- **Creation identity (#302 #304)** is reachable only on the host-authenticated
  stream. The non-console branch dispatches it at
  `internal/daemon/daemon.go:1045`. It goes through the same `d.handle` policy,
  then `hostIdentityAction` (`internal/daemon/creation_identity.go:100`). The
  context value is set only there (`internal/daemon/actions.go:317`).
- `session-identities` and `host-capabilities` are outside every restricted
  allowlist. These pass under isolation:
  - `TestRestrictedQueryDeniesIdentityQueries`
  - `TestSessionRPCShapesCarryNoIdentity`
- The provider/cloud path never sends identity frames. Only `internal/direct`
  calls `ApplyIdentity`.
- `QueryResources` (other sessions' process data) is `ScopeHost`
  (`internal/access/scope.go:98`).
- **Repo grants (#280).** A workgroup created without repos, including a
  remote new-workgroup with an empty `repos`, now gets a live "all tracked
  repos" ceiling (`internal/wsops/wsops.go:160`,
  `internal/store/grants.go:28`). Coordinators also see every tracked repo name,
  with granted marked (`internal/daemon/session_policy.go:274`). This stays
  within one host owner, and add-agent still enforces authority. There is no
  cross-user or cross-session bypass. See F1.

### First prompt, async creation, lifecycle (#275 #285 #302)
- **Typed prompt/interject on remote creation (#275).** Typed text is now held
  until the runtime process is 2s old (`internal/daemon/steer.go:185`,
  `:45`; `internal/engine/local/local.go:483`). This replaces a fixed delay
  added only when the prompt itself started the agent. Remote CREATE followed
  by a prompt is now covered. It is still a heuristic: see G1.
- **Remote CREATE from zero sessions** still runs through `wsops.Dispatch`.
  Changes on that path:
  - It now uses a cross-process id reservation (`internal/wsops/reserve.go:36`).
  - It uses a plain-INSERT `CreateSession` (`internal/store/identity.go:125`),
    which never overwrites a row.
  - No prestarted session is assumed.
- **Shutdown (#285).** ACKed deferred work is cancelled before the join
  (`internal/daemon/daemon.go:431`), and each join is bounded
  (`:710`). This removes a wedge that kept the singleton lock held. See F5.
- **Codex** releases a superseded prompt waiter
  (`internal/codexapp/supervisor.go:958`).

### Migration and concurrency (#300 #302)
- Schema migration, the incarnation column and backfill, the grant backfill
  and the legacy import each run in `BEGIN IMMEDIATE`
  (`internal/store/store.go:311`). First opens are single-flighted within a
  process (`:125`). The WAL switch is retried on `SQLITE_BUSY` (`:220`).
- An old binary that inserts rows after migration is repaired on the next
  open.

### Host identity
- Provider status and `amux doctor` show the provider id and owner.
- Daemon lock contention names the holder's pid and age.
- The direct supervisor is inert unless `direct.enabled`
  (`internal/directsvc/supervisor.go:162`).

## Classification

**Blockers:** none found.

**Nonblocking follow-ups (for root disposition):**
- **F1. Policy widening (#280).** Phone- or web-created workgroups can give
  members worktrees of *any* host-tracked repo, live. This is deliberate, but
  the beta policy should acknowledge it.
- **F2. Downgrade to old2d.**
  - Workgroups that cb6b7b2 created in all-tracked mode read as an initialized
    *empty* ceiling on old2d. Old `CheckCoordinatorRepos` then denies every repo
    on add-agent. This fails closed and is not a bypass.
  - Fix after downgrade: `amux workgroup grants <id> <repos>`.
- **F3. #279 tailer idle-skip** (`internal/runtimeevents/tailer.go:164`).
  - Trigger: the daemon publishes a permission binding after the request bytes
    were read, and no further bytes arrive before the resolution.
  - Effect: `permission_resolved` can go out without its generation
    (`internal/runtimeevents/common.go:129`).
  - The request event, which is what the cloud answer uses, is unaffected.
  - Low severity, not demonstrated.
- **F4. Provider status lock.** To replace old2d, stop the old provider first,
  or the new one exits with `ErrStatusHeld`. A service restart that overlaps
  the old process's exit can briefly loop.
- **F5. Interrupted prompts on shutdown.** A prompt that was ACKed but not yet
  delivered is now cancelled when the daemon shuts down (before, shutdown
  wedged). Clients see "accepted", not "applied".
- **F6. Documented limitations.** A crash can leak an id reservation
  (`internal/wsops/reserve.go:23`). A late creation refusal can leave an orphan
  account binding (`internal/store/identity.go:138`). Both are harmless.

**Unverified live gates (need founder-attended devices or service):**
- **G1. First prompt after remote CREATE.** On real Claude Code
  (WSL2/Mac, driven from S25), from zero prestarted sessions.
  `TestNativeClaudeSubmitsPromptSentRightAfterLaunch` skips without
  `AMUX_TEST_CLAUDE_BIN` (that fixture belongs to c60743).
- **G2. Relayed permission allow/deny end to end.** The coordinator reports
  that the service source copies and requires `runtime_generation` (fe48
  `actions/providers.go:268`, `:295`). The live round trip is still unproven.
  old2d cannot pass this gate.
- **G3. Owner echo and revoke against the live broker.** Owner echo against
  the live service. `provide revoke` against the live broker permanently
  revokes the registration, so run it only deliberately and with a disposable
  token.
- **G4. First open of a real existing store.** Daemon and CLI concurrently,
  then a downgrade check (F2).
- **G5. Unchanged invariants.** Still to confirm live: default owner-only
  policy, session isolation and impersonation execution deny. Source shows no
  change to them in this range.

## Tests run (isolated)

Command shape: `env -i` with a scratch `HOME`/`XDG_*`/`TMPDIR`, a scratch
module and build cache, no `AMUX_*` variables, and no live model or service.
All passed:

- `./harnessproto/...`
- `./internal/provider/` (whole package)
- `./internal/store/`, `./internal/access/`, `./internal/hostbackend/`,
  `./internal/engine/local/`
- `./internal/wsops/ -run 'Identity|Reserve|Grant|AllTracked|Workgroup'`
- `./internal/daemon/` with `-run` covering steer/boot, creation identity,
  restricted identity queries, repo visibility, shutdown joins, singleton
  holder and credential cache

Skipped by design: `TestOldDaemonIdentityCompat`, which needs an old source
tree and covers the direct path only, and the native Claude boot test (G1). The
stopped provider/archive experiment 6191/6194 was not rerun or reconstructed.
