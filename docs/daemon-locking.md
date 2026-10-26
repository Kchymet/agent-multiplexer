# Daemon locking

This is the audit behind splitting the daemon's global dispatch/effect
serialization into one lock per shared resource. It lists every daemon-wide
mutex, what each critical section actually reads and writes, the invariant the
lock protects there, and the lock that should own it. The last sections give
the target lock ordering and the order in which the work lands.

The goal: two sessions update their own status and reports concurrently, and
neither waits for the other, for reconcile, or for a slow host action.

## Why

Two mutexes serialize almost everything a session does:

- `sessionRuntime.dispatchMu` (`internal/daemon/session_lifecycle.go`):
  "dispatch ownership", taken by every gated mailbox dispatch, by reconcile, by
  every host action, and by mailbox shutdown.
- `Daemon.effectMu` (`internal/daemon/daemon.go`): "the final
  principal/policy/effect admission boundary", taken right after `dispatchMu`
  on all the same paths, plus account management and completion cleanup.

Both are daemon-wide. A hook's activity report for session A therefore waits
for: session B's report, the 50 ms reconcile pass over every session, and any
host action (add-agent, restart, archive) for any session for as long as that
action runs, including stopping and spawning processes.

`BenchmarkConcurrentReportsDuringSlowEffect`
(`internal/daemon/session_report_bench_test.go`) shows the cost. Two sessions
report while a holder takes the gates for 100 ms at a time:

| metric | value (master 664f1e7d) |
| --- | --- |
| p50 per report (context + activity) | ~1.3 ms |
| max per report | ~102 ms |

`TestSessionReportIndependence` (`internal/daemon/lock_independence_test.go`)
records which holders block an unrelated session's report today (see
[The harness](#the-harness)).

## Inventory of daemon-wide locks

Per-connection locks (`connState.mu`, `connState.obMu`, `client.read`) and the
per-subject `sessionrpc.Server` are not daemon-wide and are left out.

| lock | declared | guards | held across slow work? |
| --- | --- | --- | --- |
| `sessionRuntime.dispatchMu` | session_lifecycle.go | see [Holders](#holders-of-dispatchmu-and-effectmu) | yes: whole host actions, reconcile |
| `Daemon.effectMu` | daemon.go | see [Holders](#holders-of-dispatchmu-and-effectmu) | yes: whole host actions, reconcile |
| `runtimePermissionGate.mu` | session_permission.go | the subject→runtime map: identity, generation, observed/claimed permission requests | **yes**: `publish*` holds it across `create()` (`Engine.Ensure`, `codex.Ensure`: process spawn); `retireAnd` across `stop()` (`Engine.Kill`, which reaps) |
| `Daemon.authMu` | daemon.go | `authPending` (auth-reload queue); also brackets runtime kill/quiesce in `killEngineFor`, `killRuntimeFor`, `killRuntimeToken`, restore and restart | yes: kill + reap, quiesce, regrant |
| `Daemon.accountMu` | daemon.go | `accountAttempts` (login ceremonies), central Codex state | ceremony steps; network for some |
| `accountReadinessCache.mu` | account_readiness.go | cached account snapshot for creation preflight | no |
| `accounts.lock` (flock) | internal/accounts/store.go | the account store file, across processes | no (read-modify-write of one file) |
| `Daemon.mu` (RW) | daemon.go | `sessions` snapshot from the poll loop | no |
| `Daemon.subsMu` | daemon.go | snapshot subscribers | no |
| `Daemon.persistMu` | daemon.go | live-agent restore file | no (one small write) |
| `Daemon.servingMu`, `deferredMu` | daemon.go | shutdown admission for host transports and deferred work | no |
| `completionRegistry.mu` | session_lifecycle.go | pending self-completion entries | no |
| `sessionResponseBudget.mu` | session_response_budget.go | shared mailbox response byte budget | no |
| `sessionEventPager.mu` | session_events.go | runtime-event cursor cache | no |
| `sessionCaptureSlot` (chan, size 1) | session_reports.go | bounds staged transcript copies to one (256 MiB disk) | yes by design; waited on outside the gates, bounded to 5 s |
| `FileAuthority.mu` (RW) | internal/access/authority.go | credential registry and replay state | writes commit files under it (rotate, revoke, regrant) |
| `local.Engine.mu` | internal/engine/local/local.go | engine instance map | **yes**: `Ensure` holds it while it spawns the process |
| `codexapp.Manager.mu` | internal/codexapp/manager.go | supervisor map; already has a per-session `starts` creation gate | no (spawn is per session) |
| `store.sharedMu` | internal/store/store.go | the pooled `*sql.DB` per DSN | no; the pool itself is goroutine-safe, SQLite runs WAL with `busy_timeout(5000)` |

## Invariants the gates enforce

Splitting the gates is safe only if each of these survives with a narrower
lock. Each is quoted from the code that relies on it.

1. **Check-then-effect.** "A policy mutation cannot commit between a session's
   last current-generation check and admission of its exact effect"
   (`effectMu`). Every gated dispatch re-validates the principal and
   re-authorizes the canonical request under the gate, then runs the effect.
   Archive, restore, move, delete, grant changes, revocation and runtime
   replacement are the policy mutations; all take the same gate.
2. **Report correlation.** A report is written only for the runtime
   generation that is live at write time (`dispatchSessionReport`,
   `dispatchSessionCapture`). The generation comes from
   `runtimePermissionGate`; the session's effective archive state includes its
   root's (`inheritParentArchive`), so a member's fence is its own row *and* its
   root row.
3. **Mailbox lifecycle versus dispatch.** `closeServer` must not close a server
   whose `ServeOnce` is dispatching, because that dispatch may be waiting for
   the gates the closer holds; it hands the close to the pass's owner. The
   dispatch rechecks credential validity under the gates before any effect.
4. **Restore drains completion first.** `handle` drains the completion owner
   (`completions.cancelAndWait`) while holding `dispatchMu` but before
   `effectMu`, because the completion cleanup itself takes `effectMu`; holding
   `dispatchMu` keeps a call from the old principal entering between the drain
   and admission.
5. **Runtime visibility and generation move together.** `publish*` runs the
   runtime creator under the permission gate so no old-generation permission
   decision can cross the replacement; `retireAnd` removes the generation
   before the stop so nothing is consumed by a dying runtime.
6. **Deferred work re-admits.** Work acknowledged before it runs (cold start
   behind `prompt`) retakes the gates in `admitDeferredEffect` and re-runs
   `revalidateDeferred`; it never trusts the admission marker it inherited.

Observation: invariant 5 is not actually provided by `effectMu`.
`resumeWithSharedAuth` (poll loop), `startAgent` from pane open, and restore of
live agents publish runtimes without the gate; only the permission gate's own
mutex orders them against reports. The per-session lock below has to cover
those paths too.

## Holders of dispatchMu and effectMu

"Reads/writes" lists the state the critical section really touches. "Target"
uses the resource names from [Target locks](#target-locks).

| holder | entry | takes | reads / writes | target |
| --- | --- | --- | --- | --- |
| `sessionRuntime.dispatch` (generic operation) | session mailbox | both | principal validity (authority); policy over caller and target rows, membership, grants; then `d.handle` → `wsops.Dispatch` (store rows), engine/codex for start/steer | topology R (W if the verb changes membership/grants/rows) + session locks of caller and target(s) |
| `dispatch` → `dispatchSessionReport` (activity, model, limits, permission request/resolve/clear) | session mailbox | both | caller credential; caller row + root row (archive); caller's runtime generation; writes **only** caller's hook-state file, runtime-model file, permission-observation journal, or caller's binding in the account store | **caller's session lock only** (+ account store flock for limits, a leaf) |
| `dispatchSessionReport` for the context query | session mailbox | none since #207 | caller credential, generation | caller's session lock not needed (read of one generation) |
| `dispatchSessionCapture` (two short sections around the copy) | session mailbox | both, twice | as reports, plus opening the caller's transcript and publishing the caller's private stage | caller's session lock, twice |
| `withFinalAdmission` (runtime events, agent-session discovery) | session mailbox | both, before and after the page I/O | principal validity, policy over the caller and the target's rows | topology R + caller's session lock (+ target's for a cross-session read) |
| `dispatchCredential` (keychain/GitHub broker) | session mailbox | both | caller row (archive, agent); runs the bounded native credential op; re-authorizes | caller's session lock |
| `dispatchCentralCredential` | session mailbox | both | caller row, account binding (account store), central credential store | caller's session lock (+ account store, a leaf) |
| `reconcile` (every 50 ms) | run loop | both, for the whole pass | host credential (authority); all store rows; per session: credential validity, renewal (authority write), launch spec, mailbox open/close (servers map), revoke + kill for archived/deleted | host credential: authority only. Per session: that session's lock for open/renew/close/revoke/kill. Servers map stays owned by the run goroutine |
| `sessionRuntime.close` | shutdown | `dispatchMu` | servers map, completions, pager | run goroutine ownership after passes are joined; no daemon lock |
| `Daemon.handle` (host action, or session dispatch that reached it unadmitted) | host stream, session mailbox | both; drains completions between them for restore | depends on the action, below | per action, below |
| ↳ restart / recreate (`replaceSession`) | | | launch spec for one id; kill + spawn that id's runtime; permission gate entry | that session's lock |
| ↳ start (`startEngineFor`) | | | root fan-out: the target and its members' launch specs, spawn | topology R + locks of each started id |
| ↳ create (add-agent, new workgroup) via `wsops.Dispatch`, then `startAgent` | | | account readiness, new store rows, provisioning, spawn | topology W for the row insert only; then the new id's lock for start, topology released |
| ↳ archive / delete (`wsops.Dispatch` + `killEngineFor`) | | | target row, its children (store), kill of target and members | topology R (W for delete) + locks of target and every member |
| ↳ restore (`restoreSessionAccess`) | | | completion drain; target + members' credential state (authority), quiesce, regrant, store row | drain first, then topology R + locks of target and members |
| ↳ move / rename / grants (`wsops.Dispatch`) | | | rows and grant tables | topology W (move: + moved session's lock, its fence changes) ; rename: that session's lock only |
| ↳ steer / prompt (`steer`) | | | target's live runtime, permission gate consume | target's session lock |
| ↳ account manage (`accountCall`) | | `effectMu` unless status | account store, ceremonies, readiness cache, kills bound workloads on disconnect | account lock(s) + the killed session's lock |
| ↳ auth-reload queue, codex login, refresh, shutdown | | | `authPending`, codex home, poll trigger | `authMu` / none |
| `admitDeferredEffect` (cold start behind `prompt`) | deferred goroutine | both | revalidate; `startAgent` for one id; queue input | that session's lock (+ topology R inside `revalidateDeferred`) |
| `completionRegistry` stop (after self-archive) | completion goroutine | `effectMu` | caller row (archived), revoke exactly its principal, kill exactly its runtime token | that session's lock |

## Target locks

Each shared resource gets its own lock. Nothing replaces the gates with
another daemon-wide mutex on the report path.

- **Per-session lock** (`sessionLocks`, a keyed table of mutexes, one per
  session id, created on demand). Owns everything whose correctness is about
  one session: its effective fence (own archived flag, credential validity at
  the instant of an effect, runtime generation), its report files (hook state,
  runtime model, permission observations, published capture), its mailbox
  server open/renew/close/revoke, and its runtime retire/publish. Acquired for a
  set of ids at once, in ascending id order, through one helper.
- **Topology lock** (`topologyMu`, `sync.RWMutex`). Owns workgroup structure
  and grants: which rows exist, `root_id` membership, coordinator repo grants,
  provider grants. Read-locked by cross-session authorization and by group
  fan-out (so the member set it locks cannot change underneath it);
  write-locked only for the short store mutation that changes structure
  (create, delete, move, grant). Never held across a process spawn, a kill, or
  network I/O. Self-reports never take it: a report's fence is the caller's row
  and its root's archived flag, and every operation that changes either holds
  the caller's session lock (root archive/restore locks every member).
- **Credential authority**: `FileAuthority.mu`, unchanged. A leaf. Host
  credential renewal needs nothing more because only reconcile performs it.
- **Permission gate**: `runtimePermissionGate.mu` shrinks to guarding the map
  for the duration of a lookup or swap. Publication and retirement of subject X
  are serialized by X's session lock, which every publisher and retirer
  (including `resumeWithSharedAuth`, pane open and restore) must hold; `create`
  and `stop` then run outside the map mutex. Invariant 5 holds per subject.
- **Engine instance map**: `local.Engine.mu` (leaf). Its `Ensure` holds the map
  lock across spawn, which serializes every runtime start daemon-wide. That is
  inside `internal/engine/local`; it is listed here and narrowed in its own PR
  (a per-key creation gate like `codexapp.Manager.starts`).
- **Account/usage cache**: `accountMu` (ceremonies), `accountReadinessCache.mu`
  and the `accounts.lock` flock stay as they are, as leaves. `accountCall` stops
  taking a global gate; a disconnect that kills bound workloads takes those
  sessions' locks for the kill.
- **Store**: no daemon lock. The shared pool is goroutine-safe and SQLite (WAL,
  5 s busy timeout) serializes writers. Multi-row invariants use store
  transactions, not daemon mutexes.
- **Auth reload queue**: `authMu` keeps `authPending` only. Kills move under the
  session lock; `authMu` is never held across a kill or spawn.
- **Snapshot, subscribers, live-agent file, serving/deferred admission,
  completion registry, response budget, event pager**: unchanged leaves.

## Lock ordering

Acquire in this order; never acquire an earlier lock while holding a later
one.

1. **Completion drain** (`completions.cancelAndWait`) happens before any lock
   below, because the cleanup it waits for takes the session lock.
2. **`topologyMu`** (read or write).
3. **Session locks**, as one ascending-id set. Never take a second session's
   lock while holding one except by releasing and reacquiring the whole set.
4. **`authMu`**.
5. **Leaves**, in no particular order and never nested in each other:
   `runtimePermissionGate.mu`, `FileAuthority.mu`, `local.Engine.mu`,
   `codexapp.Manager.mu`, `accountMu`, `accountReadinessCache.mu`, the
   `accounts.lock` flock, `completionRegistry.mu`, `Daemon.mu`, `subsMu`,
   `persistMu`, `sessionEventPager.mu`, `sessionResponseBudget.mu`, the store
   pool.

Rules that go with it:

- Session reports take exactly one session lock, their own, plus at most the
  account flock.
- Do not wait for another goroutine while holding a lock it may need. The
  `closeServer` hand-off (close after the in-flight pass finishes, on the
  pass's goroutine) is the pattern; keep it.
- Do not hold `topologyMu` for writing across a spawn, a kill, a wait on a
  runtime, or network I/O. Do the structural store change under it, release
  it, then do per-session work under session locks.
- `create`/`stop` callbacks passed to the permission gate run under the
  caller's session lock, not under the gate's map mutex.
- A session lock may be held across that session's own process spawn or kill.
  That delays only that session.
- During migration `dispatchMu`/`effectMu` sit at position 2.5 (after
  `topologyMu`, before session locks): an operation may still take them
  around session locks, never inside one.

## Migration

One resource at a time, each a small PR that keeps `-race` clean, flips the
matching harness entries, and records the two benchmarks before and after.

1. This audit and the harness.
2. **Per-session report path.** Add `sessionLocks`. Self-reports (activity,
   model, limits, permission observations) and capture's two sections take only
   the caller's session lock. Every operation that changes a session's fence
   (archive/restore/delete/move of it or its root, revoke, runtime
   publish/retire, completion cleanup) takes that session's lock in addition to
   the gates. Shrink the permission gate's map mutex so publication holds the
   session lock instead. Flips "b's report waiting on the account store" and
   "b's runtime publication".
3. **Reconcile.** Read rows with no lock; open/renew/close/revoke each session
   under its own lock; drop the global pass. Flips "reconcile opening b".
4. **Host actions.** Each action takes `topologyMu` and the session locks of
   what it touches instead of the gates; generic session dispatch likewise.
   Remove `dispatchMu` and `effectMu`. Flips "host restart of b".
5. **Engine spawn.** Per-key creation gate in `internal/engine/local`.

## The harness

`TestSessionReportIndependence` starts one holder at a time on session b's (or
the daemon's) behalf, then sends session a's activity report. Each entry in
`lockHolders` records whether a's report waits for it today:

| holder | blocks a today | why |
| --- | --- | --- |
| nothing held (control) | no | |
| account store lock alone (control) | no | activity never touches the account store |
| host restart of b | yes | `handle` holds both gates while it resolves b's launch |
| reconcile opening b | yes | the whole pass holds both gates |
| b's report waiting on the account store | yes | b holds both gates while its limits write waits for the flock |
| b's runtime publication | yes | the permission gate mutex is held across b's runtime creation, and a's report reads its generation under it |

A PR that frees a resource flips its entries to `false`; the test fails if an
entry claims blocking that no longer happens, so the table stays current.
