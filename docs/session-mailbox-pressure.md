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

## Second, independent defect: first credential attempt always misses

A session's mailbox server is opened and `service.json` is published only by
the next `reconcile` pass in the run loop. That pass needs the dispatch/effect
admission locks, which the creating/starting action holds until it returns.
The PTY launcher starts inside that action. Its first fetch reads no
`service.json`, fails immediately with "mailbox request failed", and sleeps
`LaunchRetry` (5s). Every new central Claude session therefore waits ≥5s even on
an idle daemon.

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

## Fix (in progress on this branch)

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
2. **Publish before launch.** The mailbox service is published before the
   runtime is started, so the launcher's first fetch is served.
