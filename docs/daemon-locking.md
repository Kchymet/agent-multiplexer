# Daemon locking

The daemon serializes nearly all of its effects behind two daemon-wide
mutexes: `sessionRuntime.dispatchMu` and `Daemon.effectMu`. Session A's status
report therefore waits for session B's report, for every 50ms reconcile pass,
and for any slow host action such as `add-agent` or `restart`, even though none
of them touch A's state. This document lists what those locks guard today, the
per-resource lock that should own each piece, and the order the PRs move them
in. Each PR updates this file and the row it flips in
`TestSessionReportIndependence`.

## Measured today

`BenchmarkConcurrentReportsDuringSlowEffect` runs two sessions' report pairs
(context query + activity) while a host `start` of a third session repeatedly
takes 100ms inside the real host action admission path.
`BenchmarkSessionReportDispatch` is one report with no contention.

| Benchmark | Result (master 664f1e7) |
| --- | --- |
| ConcurrentReportsDuringSlowEffect | p50 1.3ms, max 102ms |
| SessionReportDispatch activity/shared | 0.24ms/op |
| SessionReportDispatch context/shared | 0.18ms/op |

`TestSessionReportIndependence` (internal/daemon/session_lock_independence_test.go)
records which slow holders delay a report from session `a1`:

| Slow holder | Blocks a1's report today | Unblocked by |
| --- | --- | --- |
| a2's report, stalled inside its write | yes | PR 2 |
| host `start` of a3, stalled in its effect | yes | PR 4 |
| reconcile pass, stalled partway | yes | PR 3 |

## What the global gate protects

`effectMu` is documented as the "final principal/policy/effect admission
boundary": a policy change (archive, revocation, membership or grant change,
runtime replacement) must not commit between a caller's last authorization
check and the effect it authorized. `dispatchMu` sits outside it. Its only job
beyond `effectMu` is the explicit-restore drain: `handle` holds it while it
waits for a pending completion owner to finish, so no old-principal dispatch can
enter between that drain and the restore's own admission.

Neither lock protects in-memory data that lacks its own lock. Every structure
the critical sections touch is already safe on its own (see Leaf locks), so the
gate only enforces check-then-act atomicity. That is what makes the split
possible: each critical section needs atomicity only over the resources its
check reads and its effect writes.

### Holders of dispatchMu and effectMu

| # | Holder | Locks | Check reads | Effect writes | Target lock |
| --- | --- | --- | --- | --- | --- |
| 1 | `sessionRuntime.reconcile` (session_lifecycle.go), every 50ms tick and at start | both | host credential; all store sessions; each session's credential | host credential rotation; per session: credential renew/rotate, mailbox server open/close (`servers`, `initialized`, `serving`, `closing` maps, owned by the run goroutine), console accounts listener, revoke + `killRuntimeFor` | no gate for the scan; session lock per session it changes; host credential under the authority's own lock |
| 2 | `sessionRuntime.dispatch`, report actions (activity, model, limits, permission-*) | both | authority `Valid`; own session row and its root's archive flag (`resolver.Lookup`, `lookupSession`); own runtime generation (`permissions`) | own hook-state file, own runtime-model file, own permission-observation journal (read-check-append); `accounts.ObserveClaude` (accounts state, flock'd) | session lock (self) |
| 3 | `sessionRuntime.dispatch`, every other operation | both | `policy.Authorize`: membership, grants, archive state across the caller's workgroup | restricted queries (read only); self-completion (archive self, `completions.begin`); `d.handle` for the canonical action | topology read + the locks of the action's targets (as for host actions) |
| 4 | `withFinalAdmission` (session_dispatch.go), twice per runtime-events page | both | authority `Valid`, `policy.Authorize` | none: it gates disclosure | topology read + session lock of the paged subject |
| 5 | `dispatchSessionCapture` (session_reports.go), two short gates around the copy | both | as #2, plus the session's dir and Claude id | publishes the staged transcript of self | session lock (self) |
| 6 | `dispatchCentralCredential` (central_credential.go) | both | own session row, accounts binding/profile, central token store | `accounts.Accept` (binding delivery state) | session lock (self) |
| 7 | `dispatchCredential` (credential_broker.go) | both | own session row | runs a keychain or GitHub credential helper process: slow and external | session lock (self) |
| 8 | `Daemon.handle` (actions.go), all host actions and #3's actions | both, unless already admitted | `revalidateDeferred` (caller's guard) | anything: store mutations through wsops, engine start/kill/restart, authority restore/regrant, account manage, steer, shutdown | per action: the locks of the resources it names (see Target model) |
| 9 | `admitDeferredEffect` (steer.go), deferred start behind an accepted `prompt` | both | `revalidateDeferred` | `startAgent`, queue input on the instance | session lock of the target |
| 10 | `accountCall` (accounts.go), non-status operations when not admitted | effectMu | caller guard | `readiness` cache clear, account attempts (`accountMu`), accounts state | account lock (today `accountMu`) |
| 11 | `completionRegistry.finish` (session_lifecycle.go) | effectMu | completion ownership, archive flag | `RevokeCurrent`, `killRuntimeToken` | session lock (self) |
| 12 | `sessionRuntime.close` | dispatchMu | — | closes every server after all passes joined | none once the run goroutine owns shutdown |

The central Codex token delivery (central_codex.go) already avoids the gate:
the codexapp per-session gate is sometimes taken below `effectMu`, so delivery
uses `authority.WithValid` (an authority read lease) plus the accounts CAS as
its admission point. That pattern, a check and a short effect under the lock of
the one resource that can invalidate it, is the model for the split.

Some runtime replacements already happen outside the gate. Start and restart
publish a new runtime generation under it, but pane open (`paneOpen`) and the
auth-reload resume on the poll goroutine (`resumeWithSharedAuth`, under
`authMu`) publish one without it. Permission observations and limits are keyed
by generation, so a stale one lands in a record nobody reads. Hook state and
the runtime model are keyed by session and Claude id, which survive a resume,
so a stale activity report can overwrite the replacement's state until that
runtime's next report. The session lock must cover these two paths too, so
that a report which checked generation G cannot write after G is retired.

### Leaf locks

These guard their own data and never call out while held. They stay as they
are and sit below every resource lock in the order.

| Lock | Guards |
| --- | --- |
| `Daemon.mu` (RW) | `sessions` snapshot slice |
| `Daemon.subsMu` | snapshot subscribers |
| `Daemon.persistMu` | live-agents file and `lastLive` |
| `Daemon.authMu` | engine/codex instance kill and restart, `authPending`; taken by `killRuntimeFor`, `killEngineFor`, `killRuntimeToken`, auth reload |
| `Daemon.accountMu` | account attempts/ceremonies |
| `accountReadinessCache.mu` | cached account snapshot |
| `Daemon.servingMu`, `Daemon.deferredMu` | host transports and deferred work admission |
| `runtimePermissionGate.mu` | runtime generations and permission claims; `retireTokenAnd` runs its callback under it (called with `authMu` held) |
| `completionRegistry.mu` | completion entries |
| `sessionEventPager.mu`, response budget `mu` | event paging state, mailbox response budget |
| `FileAuthority.mu` (RW) | credential registry and replay ledger; `WithValid` holds it shared during a short callback |
| `store` `sharedMu`; SQLite | pooled handles; SQLite WAL with `busy_timeout(5000)` serializes writers itself |
| accounts state flock | `accounts.Update` read-modify-write of the accounts file |
| codexapp manager per-session gate, engine `local` mutexes | one runtime's supervisor/instance |

Per-session files (hook state, runtime model, permission observations,
transcript stage) have no lock of their own. `WriteSessionHookState` writes a
fixed `<path>.tmp`, and the permission journal is check-then-append, so two
concurrent writers for the same session would race. Today the gate and the
per-subject mailbox (one `ServeOnce` per subject at a time) prevent that; the
session lock takes over.

## Target model

| Level | Lock | Key | Mode | Owns |
| --- | --- | --- | --- | --- |
| 1 | topology | daemon | RW | workgroup membership, grants and repo ceilings that one session's authorization reads from other sessions' rows |
| 2 | workgroup | root id | RW | the root's archive flag (members inherit it), member add/remove |
| 3 | session | session id | exclusive | one session's store row, credential generation, runtime instance, mailbox server, and per-session files |
| 4 | leaf | — | — | the leaf locks above |

Which operations take what:

- Session reports, capture publish, credential and central-credential delivery,
  completion finish, deferred start: workgroup(root) read, session(self).
- Reconcile: no daemon-wide lock for the scan. For each session it opens,
  renews, closes or revokes: workgroup(root) read, session(id).
- Start, restart, recreate, steer, rename, kill of one agent: workgroup(root)
  read, session(id). On a root id, workgroup(root) write.
- Archive or restore: of an agent, workgroup(root) read + session(id); of a
  workgroup, workgroup(root) write.
- Add-agent: prepare the worktree and spec with no lock; commit the row under
  workgroup(root) write; start under session(new id).
- Move: topology write (short; it changes two workgroups' membership).
- Grants and repo scope changes, add/rm repo, new workgroup commit, auth
  reload, account manage: topology write, never held across slow work.
- Session-originated actions and runtime-event admission: topology read, then
  the action's own locks.
- An action the classifier does not know: topology write (the old global
  behavior), so a forgotten action is slow rather than unsafe.

### Lock order

1. Acquire levels in order: topology, then workgroups, then sessions, then leaf
   locks. Never acquire a higher level while holding a lower one.
2. Within a level, take every key you need in one call, sorted ascending by id.
   Do not add a key once a level is held; release and start again.
3. Never upgrade read to write. Release, reacquire in write mode, and recheck.
4. Drain before you lock: `completions.cancelAndWait(id)` runs before taking
   session(id), because `finish` takes session(id). After the lock is held,
   `completions.cancel(id)` (no wait) retires anything registered meanwhile;
   `finish` rechecks ownership under the lock and returns.
5. Deferred work never inherits a held lock. `admitDeferredEffect` reacquires
   from scratch, as today.
6. A topology or workgroup write lock never covers slow work: git worktree
   creation, process start or exit waits, helper processes, or network. Do it
   before the lock and commit under it. Under a session lock, slow work delays
   only that session.
7. Codex token delivery never takes a session lock for its own session (the
   codexapp per-session gate can be held below it); it keeps the authority read
   lease.
8. Run the check and the effect under the same acquisition. A lock released
   between them repeats the check after reacquiring.

### Transition

The gate cannot disappear in one step: until every `effectMu` holder is
classified, an unclassified holder must still exclude everyone. During the
transition `effectMu` becomes a read/write gate:

- Unconverted holders take it in write mode. They exclude every converted
  holder, as today.
- Converted holders take it in read mode, then their resource locks. They run
  in parallel with each other, and serialize only on shared resources.
- `dispatchMu` stays exclusive for unconverted holders only. Converted holders
  skip it: they never begin a completion, the one effect the restore drain
  fences.

Once nothing takes the gate in write mode, it and `dispatchMu` are deleted.

## PR sequence

1. This audit, the independence test, and a benchmark holder that uses the real
   host action path.
2. Session reports (#2, #5): gate read + session(self). Flips the peer report
   row.
3. Reconcile (#1): per-session work under session locks, scan with no gate.
   Flips the reconcile row.
4. Host actions (#8, #9, #11): the per-action lock classifier, with the
   topology-write fallback. Flips the host action row. This may take more than
   one PR (lifecycle actions first, then creation with work moved before the
   lock, then account manage).
5. Session-originated dispatch, credential paths, runtime-event admission (#3,
   #4, #6, #7, #10).
6. Delete the gate and `dispatchMu`.
