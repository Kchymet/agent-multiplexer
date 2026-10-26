# Session mailbox pressure: delayed credential delivery and 10/30/35s timeouts

Implementation record (base `3397deb`, which already contains #196's
concurrent per-subject serving). It describes a defect and its fix; the source
is authoritative once merged.

## Symptom

New Claude sessions print "The daemon did not answer the protected session
mailbox request within 10 seconds" before eventually starting. Claude
`UserPromptSubmit` hooks time out at 30s and coordinator `runtime-events`/status
calls time out at 35s. Meanwhile host-stream status still answers immediately.

## Root cause: consumed responses are never retired

Every signed mailbox request reserves a lease in one daemon-global response
budget (`newSessionResponseBudget`: 128 files, 8 MiB, 128 receipt slots shared by
all subjects). It reserves before verification or dispatch. A committed
response file keeps its lease until `cleanupResponses` deletes it by age,
`access.MaxResponseAge` = 60s. The client's responses mount is read-only, and no
protocol message tells the daemon that a response has been read. So the daemon
can sustain at most about **128 / 60s ≈ 2.1 requests/s across all sessions
combined**. Each Claude hook and each event or status read is one request.

Once the budget is full, `reserveResponse` returns `ErrResponseCapacity`. The
claimed request stays in `processing/` and that subject's `ServeOnce` fails on
every pass until the oldest retained responses age out. Whichever subject polls
first after a lease is released wins it, with no fairness between subjects.
Callers therefore wait up to ~60s, and each client gives up at its own deadline
(launcher 10s, hooks 30s, CLI 35s).

Replay protection does **not** depend on these files. `FileAuthority.Verify`
records every accepted request ID in the replay ledger until the request's
signed expiry (≤ `MaxRequestAge`, 30s). A request is never re-dispatched because
its response file exists.

## Second, independent defect: first credential attempt can miss publication

A session created after the runtime started gets its mailbox server opened, and
`service.json` published, only by a later `reconcile` pass in the run loop.
That pass needs the dispatch/effect admission locks, which the creating/starting
action holds until it returns. The PTY launcher starts inside that action. In
the fixture below (session stored and started while the runtime is serving), the
launcher's first fetch consistently read no `service.json`, failed immediately
with "mailbox request failed", and slept `LaunchRetry` (5s). Whether a given
production launch loses this race depends on namespace startup time versus the
next reconcile (≤50ms after the action returns).

## Evidence (real runtime: `runtime.start`/`run`, real global budget, real
## protected-namespace launcher, legitimate signed requests)

Harnesses: `internal/daemon/session_first_delivery_measure_test.go` and
`session_mailbox_pressure_measure_test.go` (opt-in by environment, see each
test).

| Scenario | Result |
| --- | --- |
| Idle daemon, fresh session started by a real `start` action | first fetch fails ("mailbox request failed"), delivered at 5.07s |
| Same, plus an unrelated admitted effect holding the gate 3s | delivered 5.07s (hidden by the 5s retry) |
| Same, gate held 12s | exact user text "did not answer … within 10 seconds", delivered 15.08s |
| 6 subjects each issuing a query every 3s (~2 req/s total) | budget files=18, no stall, background p90 119ms; credential 5.10s |
| 6 subjects every 250ms | budget **files=128, bytes=82 KB** within 8s; credential delivered **53.3s** after one transport failure and one 10s timeout; background max 18.8s, 6 calls hit the 35s deadline |

The binding limit is the file count held by already consumed responses, not
bytes or in-flight work.

Negative results kept for the record: reconcile with 40 live / 300 archived
sessions takes 3.4ms. A real `add-agent` holds the gate 90ms (empty template)
or 330ms (26 MB Claude config template, small repo). Startup response-budget
adoption cannot wedge: `Existing` adoption bypasses limits. The #198 account
usage network reads run outside the accounts flock.

## Fix

1. **Authenticated retirement.** After the client reads a response (and after
   any required receipt is settled), it publishes a signed `release` call
   naming that request ID and response digest. The server verifies it like any
   request (signature, current generation, replay ledger). The call needs no
   response reservation and publishes no response. The server deletes only a
   matching response of its own subject whose receipt is not pending, then
   releases the lease. Steady-state occupancy becomes the number of in-flight
   requests, not 60s of history. Old clients and crashed clients still fall back
   to the unchanged 60s age cleanup. Every bound (files, bytes, receipts, queue,
   replay ledger) is unchanged.
2. **Release behind a blocked request.** `ServeOnce` no longer aborts at the
   first capacity-blocked request. It keeps the blocked request claimed for a
   later pass, still processes releases (and receipts) in the same pass, and
   then reports `ErrResponseCapacity`.
3. **Receipts are one-way.** The client never reads a receipt's result
   (`sendReceipt` is fire-and-forget), so a receipt reserves no capacity and
   publishes no response, whether or not the pool is full. Previously each
   mandatory operation left an unread receipt response holding a slot for 60s.
   Receipts are still verified, authorized and settled exactly as before, and
   settlement remains the only outcome. An accepted receipt also retires the
   response it proved was read. A release never retires a response whose
   receipt is pending.
4. **Restart keeps committed responses.** `Client.awaitResponse` checks a
   response against the service the request was sent to, before it looks at
   the current boot. A client suspended across a restart therefore still reads
   its committed pre-restart success. Prior-boot responses stay adopted under
   the existing bounded accounting and expire by age. The restarted daemon does
   not dispatch the request again, and a release signed for the old boot is
   rejected by `Verify`. Releasing clients keep the retained set near the
   in-flight count, so a restart between two new daemons adopts few
   responses. This does not remove every restart delay; see the upgrade limit
   below.
5. **Launcher waits for publication within its attempt.** If `service.json` is
   absent (`sessionrpc.ErrUnpublished`; nothing was sent, so no outcome is
   uncertain), the launcher polls for publication inside its existing 10s
   attempt instead of failing and sleeping 5s. Mailbox ownership stays
   unchanged: the run loop is still the only goroutine that opens, initializes,
   serves and closes servers. Opening from `startAgent` was rejected because
   `serveCurrent`/`startPass` read those maps without the lock, and
   `restoreLiveAgents` calls `startAgent` outside the gate. If publication does
   not happen within 10s, the launcher prints a specific "not published"
   message.

### Compatibility

A new client talking to an old daemon gets an `invalid_call` response to its
release. That response is retained like any other, which doubles occupancy
until the daemon is restarted. Panes bind-mount the daemon's own executable,
and a daemon restart relaunches live agents, so after "install + daemon
restart" every client and server is new. The service record is unchanged: no
signed-field or JSON shape change breaks existing in-session clients.

### After the fix (same harnesses)

| Scenario | Before | After |
| --- | --- | --- |
| Session created while serving, idle daemon | 5.07s, retry guidance | 114ms, first attempt, no guidance |
| Old (non-releasing) client pressure, 6×250ms | 53.3s | 53.3s (expected: bounded only by the 60s TTL; one run did not deliver within the 120s harness deadline, so results vary) |
| Unrelated effect holding the gate 12s | 15.1s | 15.1s (not addressed here, see below) |
| 6 real in-namespace launchers looping credential fetches (240 releasing fetches, ~120 req/s) plus a fresh launch | n/a: stalls once 128 responses are retained (negative control: 38/40 fetches, then 90s deadline) | every fetch succeeds; fresh first attempt 159ms; pool peak 11 files |

Asserted regressions: `internal/sessionrpc/release_test.go` (production
`Client`: sustained calls through a 1-slot pool; six subjects sharing a 4-slot
pool, 240 calls in total, each under 2s, and the pool drains to zero; a release behind a
capacity-blocked request; exact digest and replay; a pending receipt plus
receipt settlement on a full pool; repeated mandatory receipts through a 1-slot
pool and through a 4-slot pool with spare capacity; a client held, with a
barrier, after an empty poll and before its next read across a restart reads
its committed response with no second dispatch), and `central_launch_runtime_test.go` (real protected-namespace
launcher: three lifecycle orders, plus six sessions of real releasing launcher
traffic through the daemon-global budget; each delivery is on the first attempt
and under `LaunchRetry`, and the pool peaks at ≤ 32 files). Each regression was
confirmed to fail with its fix reverted.

### Upgrade limit: retained pool from an older daemon

Keeping committed prior-boot responses is deliberate: a client waiting across
the restart may still read its success. The cost shows on the first upgrade
from a daemon without this change. That daemon never retired consumed
responses, so it can leave up to the full 128-file pool on disk. The new daemon
adopts all of it, because adoption ignores the limits, and admits nothing new
until age cleanup drains it. Credential fetches from relaunched agents can
therefore wait up to `access.MaxResponseAge` (60s) after that first startup.
Releasing clients prevent this from recurring on later restarts. The pool can
still be full at a later restart only if non-releasing (old) clients, or
crashed clients that never released, filled it.

### Not addressed here

Any admitted effect that holds `dispatchMu`/`effectMu` (creation, restart,
archive) still delays every session's mailbox publication and gated
dispatches, including credential delivery, for its duration. The 12s row
above shows this. Measured creation holds are short (90–330ms); no ≥10s
holder was identified in current source, and it overlaps the auth
runtime-boundary work.
