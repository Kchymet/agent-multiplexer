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
(context query + activity) while a third session repeatedly stalls for 100ms
on a real path: `host-action` is a host `start` through the host action
admission, `peer-report` is that session's own report stalled in its write,
`reconcile` is a reconcile pass stalled while it opens that session's mailbox.
`BenchmarkSessionReportDispatch` is one report with no contention.

| Benchmark | Before split (664f1e7) | After |
| --- | --- | --- |
| ConcurrentReportsDuringSlowEffect/host-action | p50 1.3ms, max 102ms | p50 0.49ms, max 1.5–2.0ms (PR 4) |
| ConcurrentReportsDuringSlowEffect/peer-report | p50 0.9ms, max 104ms | p50 0.45ms, max 2.7ms (PR 2) |
| ConcurrentReportsDuringSlowEffect/reconcile | p50 1.3ms, max 102.5ms | p50 0.45–1.1ms, max 2.1–2.6ms (PR 3) |
| SessionReportDispatch activity/shared | 0.25–0.3ms/op | 0.27ms/op |
| SessionReportDispatch context/shared | 0.18–0.2ms/op | 0.19ms/op |

`TestSessionReportIndependence` (internal/daemon/session_lock_independence_test.go)
records which slow holders delay a report from session `a1`:

| Slow holder | Blocks a1's report today | Unblocked by |
| --- | --- | --- |
| a2's report, stalled inside its write | no | PR 2 |
| host `start` of a3, stalled in its effect | no | PR 4 |
| host `start` of the workgroup root, stalled resolving its members | no | PR 4, workgroup-start slice |
| host archive of a3, stalled stopping its runtime | no | PR 4, archive slice |
| one-key archive toggle of a3 (may restore: still exclusive) | yes | restore slice |
| host `agent-set-repos` of a3, stalled in its git work | no | PR 4, repo slice |
| host `add-repo`, stalled cloning | no | PR 4, repo slice |
| the coordinator's own `add-agent` (through its mailbox), stalled creating | no | PR 4, session-originated slice |
| a3's completion cleanup, stalled stopping its runtime | no | PR 4, completion slice |
| archive of a1's own workgroup | yes, and a1 is then refused | correct: a1 is archived |
| an exclusive action queued behind a slow `start` of a3 | no | PR 4 (try-and-retry writers) |
| a2's GitHub credential read, stalled in its helper | no | PR 5 |
| host `new-workgroup`, stalled launching its coordinator | no | PR 4, creation slice |
| host `start` of a3, stalled inside its runtime launch (`engine.Ensure`) | no | PR 7 (permission gate per subject) |
| reconcile pass, stalled while it opens a3's mailbox | no | PR 3 |

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
| 1 | `sessionRuntime.reconcile` (session_lifecycle.go), every 50ms tick and at start | both | host credential; all store sessions; each session's credential | host credential rotation; per session: credential renew/rotate, mailbox server open/close (`servers`, `initialized`, `serving`, `closing` maps, owned by the run goroutine), console accounts listener, revoke + `killRuntimeFor` since PR 3: no lock for the scan or the lock-free pre-check; gate shared + session(id) for each session it changes, rereading the row there; host credential under the gate shared. Tick passes try-acquire and leave a busy session to the next tick |
| 2 | `sessionRuntime.dispatch`, report actions (activity, model, limits, permission-*) | gate shared + session(self) since PR 2 | authority `Valid`; own session row and its root's archive flag (`resolver.Lookup`, `lookupSession`); own runtime generation (`permissions`) | own hook-state file, own runtime-model file, own permission-observation journal (read-check-append); `accounts.ObserveClaude` (accounts state, flock'd) | session lock (self) |
| 3 | `sessionRuntime.dispatch`, every other operation | since PR 5: restricted queries gate shared; `sessionScopedAction` actions (a coordinator steering a member) go to `handle`'s session admission after a shared pre-check; everything else both (exclusive by retry) | `policy.Authorize`: membership, grants, archive state across the caller's workgroup | restricted queries (read only); self-completion (archive self, `completions.begin`); `d.handle` for the canonical action | topology read + the locks of the action's targets (as for host actions) |
| 4 | `withFinalAdmission` (session_dispatch.go), twice per runtime-events page | gate shared since PR 5 | authority `Valid`, `policy.Authorize` | none: it gates disclosure | topology read + session lock of the paged subject |
| 5 | `dispatchSessionCapture` (session_reports.go), two short admissions around the copy | gate shared + session(self) since PR 2 | as #2, plus the session's dir and Claude id | publishes the staged transcript of self | session lock (self) |
| 6 | `dispatchCentralCredential` (central_credential.go) | gate shared + session(self) since PR 5 | own session row, accounts binding/profile, central token store | `accounts.Accept` (binding delivery state) | session lock (self) |
| 7 | `dispatchCredential` (credential_broker.go) | gate shared + session(self) + `credentialMu` (leaf, serializes helper processes) since PR 5 | own session row | runs a keychain or GitHub credential helper process: slow and external | session lock (self) |
| 8 | `Daemon.handle` (actions.go), all host actions and #3's actions | since PR 4: gate shared + session(id) for `sessionScopedAction`; otherwise both (exclusive by retry), unless already admitted | `revalidateDeferred` (caller's guard) | anything: store mutations through wsops, engine start/kill/restart, authority restore/regrant, account manage, steer, shutdown | per action: the locks of the resources it names (see Target model) |
| 9 | `admitDeferredEffect` (steer.go), deferred start behind an accepted `prompt` | gate shared + session(id) since PR 4 | `revalidateDeferred` | `startAgent`, queue input on the instance | session lock of the target |
| 10 | `accountCall` (accounts.go), non-status operations when not admitted | effectMu | caller guard | `readiness` cache clear, account attempts (`accountMu`), accounts state | account lock (today `accountMu`) |
| 11 | `completionRegistry.finish` (session_lifecycle.go) | gate shared + session(self) since the completion slice | completion ownership, archive flag | `RevokeCurrent`, `killRuntimeToken` | session lock (self) |
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
| `Daemon.creationMu` | session creation (`wsops` worktree and row setup), under the gate shared; taken after any session lock, and released by a creation before it locks its new session |
| `Daemon.credentialMu` | host credential helper processes (keychain, `gh`), below a session's admission |
| `accountReadinessCache.mu` | cached account snapshot |
| `Daemon.servingMu`, `Daemon.deferredMu` | host transports and deferred work admission |
| `runtimePermissionGate.mu` + per-subject locks | runtime generations and permission claims. Since PR 7 the subject lock is held across the callbacks (runtime creation in `publish`, kill in `retireAnd`/`retireTokenAnd`, delivery in `consume`), and `mu` only for map access. Before, one global `mu` was held across `engine.Ensure` and kills, so every runtime start or stop stalled every session's reports in `generation()`. Callers' locks (session, `authMu`) come before the subject lock |
| `completionRegistry.mu` | completion entries |
| `sessionEventPager.mu`, response budget `mu` | event paging state, mailbox response budget |
| `FileAuthority.mu` (RW) | credential registry and replay ledger; `WithValid` holds it shared during a short callback |
| `store` `sharedMu`; SQLite | pooled handles; SQLite WAL with `busy_timeout(5000)` serializes writers itself |
| accounts state flock | `accounts.Update` read-modify-write of the accounts file |
| codexapp manager per-session gate, engine `local` mutexes | one runtime's supervisor/instance |

Per-session files (hook state, runtime model, permission observations,
transcript stage) have no lock of their own. `WriteSessionHookState` writes a
fixed `<path>.tmp`, and the permission journal is check-then-append, so two
concurrent writers for the same session would race. The session lock
(`Daemon.sessionLocks`) guards them, alongside the per-subject mailbox (one
`ServeOnce` per subject at a time).

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
- Any code that resolves a workgroup's members under the shared gate must act
  only on the set it resolved and locked: creations add members concurrently.
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
- A goroutine that serves other sessions (the reconcile tick) never queues for
  the gate or a session lock. It try-acquires and skips. A queued writer on a
  `sync.RWMutex` also stalls every new reader, so exclusive holders that may
  wait behind a slow shared holder should move to try-and-retry acquisition
  (#210 did this) as they are classified in PR 4.

Once nothing takes the gate in write mode, it and `dispatchMu` are deleted.

## PR sequence

1. This audit, the independence test, and a benchmark holder that uses the real
   host action path.
2. Session reports (#2, #5): gate read + session(self) through
   `admitSession`, with `keyedLocks` for the per-session locks. Flips the peer
   report row. Done.
3. Reconcile (#1): store scan and a lock-free pre-check with no lock (an
   archived session with no server, credential or runtime is skipped instead of
   revoked and killed on every tick). Each session it changes is handled under
   `admitSession`, with a reread of its row. Tick passes use
   `tryAdmitSession`. Flips the reconcile row. Done.
4. Host actions (#8, #9, #11), in slices.
   - First slice, done: `sessionScopedAction` sends `start`, `restart`,
     `recreate` and `steer` of one existing session (not a workgroup start) to
     `admitSession`. Every other action keeps the exclusive gate. Steer's
     deferred admission (`admitDeferredEffect`) takes the target's admission.
     Every exclusive acquisition (`lockEffectExclusive`) retries `TryLock`
     instead of queueing, so a writer waiting behind a slow shared holder never
     stalls new readers. Flips the host action row.
   - Creation slice, done: `add-agent`, `new-workgroup` and `create-workspace`
     (`creationAction`) run under the gate shared plus `creationMu`, which
     serializes creations among themselves. Adding a session changes no
     existing session's authorization, and archive, grants and moves stay
     exclusive. The new session starts under its own lock after `creationMu` is
     released. Reconcile opens a new mailbox only while it can hold
     `creationMu` (try-acquired on ticks), so it never adopts a half-created
     row. Session-originated creations (a coordinator's `add-agent`) still use
     the exclusive path.
   - Workgroup-start slice, done: `start` of a root takes the gate shared,
     resolves its members once, locks exactly those sessions and starts only
     those (`startAgents`). Membership is not frozen by the shared gate: a
     creation adds members under `creationMu` alone. A member added after the
     resolution is started by its own creation, under its own lock.
   - Archive slice, done: explicit archive (`set-archived true`), `rename`,
     `delete` and `kill` of an agent or workgroup (`archiveScopedAction`) take
     the gate shared plus the target's session lock and, for a root, every
     member's. Archive and delete then take `creationMu` and re-resolve the
     members; if a creation added one in between, they start again. So that
     an archive or delete of a session principal cannot cross that principal's
     guard, every shared holder whose check depends on a session principal
     also locks it: restricted queries lock the caller, runtime-event
     admission locks the caller and the subject, and a session-originated
     scoped action or deferred steer locks the caller (`withGuardSubject`)
     along with the target. The one-key toggle (it may restore) and restore
     stay exclusive: restore drains completions under `dispatchMu`.
   - Repo slice, done: `add-repo` (a clone plus a repo home session, so a
     creation) and `agent-set-repos` of one agent (that agent's lock) run
     under the gate shared plus `creationMu`, which also serializes their git
     work with creations and deletions. `refresh` only wakes the poller and
     takes no gate at all. `rm-repo` and `coordinator-set-repos` (they change
     many sessions' authorization) stay exclusive.
   - Session-originated slice, done: an action a session sends (a
     coordinator creating, archiving, re-scoping, starting or steering a
     member) takes the same per-resource admission as the host's
     (`splitAction`), plus the caller's own session lock. The creation branch
     locks the caller during the creation, then takes {caller, new session}
     in one sorted acquisition to start it. Self-completion stays exclusive.
   - Completion slice, done: completion cleanup (`finish`: revoke the
     generation, then stop the runtime, whose kill can wait out the engine's
     kill grace) takes only its subject's admission. A restore cancels the
     entry before it takes the gate, and `finish` rechecks ownership under
     its lock.
   - Next: restore, move and the remaining policy changes, then account
     manage.
5. Session-originated dispatch, credential paths, runtime-event admission (#3,
   #4, #6, #7). First slice, done: restricted queries and runtime-event final
   admission take the gate shared (they only read, and every policy writer is
   exclusive). Credential-broker and central-credential delivery take the
   caller's session admission, plus `credentialMu` for helper processes (from
   #210). A session-originated `sessionScopedAction` goes to `handle`'s session
   admission, which reruns the caller's guard there. Remaining: other
   session-originated actions follow the host action classification (PR 4
   slices). `accountCall` (#10) moves with account manage.
6. Delete the gate and `dispatchMu`.
7. The runtime permission gate: per-subject locks across its callbacks,
   `mu` for the map only. Found while slicing archive: the gate's global
   mutex was a second daemon-wide lock behind every runtime start and stop.
   Done.
