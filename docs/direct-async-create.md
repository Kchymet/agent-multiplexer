# Direct asynchronous creation

Status: implemented (additive to `amux-direct/1`). Contract fixtures:
`docs/direct/fixtures/async-create-v1/`.

## Why

A direct creation (`add-agent`, `new-workgroup`, `new-workgroup-empty`) runs
synchronously under a 60s execution bound. Fetching a large repository or
preparing a worktree outlives it: the direct server closes its daemon socket
and records the key `idem_ambiguous`. Closing the socket does **not** stop the
daemon — it runs an accepted action to completion under its own lifetime — so
the creation usually succeeds while the client was told "unknown". Harness
relay solves the same problem with `async: true` and `daemon.create_status`;
this is the direct equivalent, with no Harness traffic, no new service and no
daemon change. It reuses the daemon call, the idempotency record and the
destination prechecks of the synchronous path, which is unchanged.

## Wire

**Discover.** `GET /direct/v1/capabilities` carries `"async_create": true` for
a grant holding `create`. An older server omits it, and refuses `async` with
`400 bad_request` (strict decode), so a client never gets an unexpected shape.

**Start.** Send the usual creation request with `"async": true`:

* `POST /direct/v1/workgroups` — `new-workgroup`, `new-workgroup-empty`;
* `POST /direct/v1/sessions/{sid}/actions` — `add-agent` only (any other verb
  with `async` is `400 bad_request`).

Every synchronous refusal is unchanged and recorded nothing: `401/403` auth,
`404` scope (the console is never in scope), `409 archived`,
`422 not_in_catalog`, `409 capacity_exhausted`, and the new
`429 rate_limited` (too many pending creations; nothing recorded, retry the
same key later). Otherwise the answer is **always `202`** with a
`CreationStatus` — never an `ActionResponse` — after up to ~2s in which a fast
creation may already finish.

**Follow.** `GET /direct/v1/creations/{creation_id}` (permission `create`, the
creating grant only; anything else is `404 not_found`) answers `200` with the
`CreationStatus`. Long-poll with `?wait=N` (seconds, capped at 20) and
`&after=V` (the last `version` seen): the answer comes when `version > V`, the
creation is terminal, or `wait` passes. Polling never extends the grant's idle
lifetime.

```json
{"creation_id":"crt_…","verb":"add-agent","phase":"ready","version":3,
 "new_id":"4c2a91","root_id":"771951","elapsed_ms":93000}
```

| phase | version | meaning |
|---|---|---|
| `queued` | 1 | recorded; waiting for an execution slot; nothing sent |
| `preparing` | 2 | handed to the daemon (fetch, worktree, config, start) |
| `ready` | 3 | created: `new_id` and `root_id` (add-agent target, or `new_id` for a workgroup) are always both set |
| `failed` | 3 | `code` + `error`; `uncertain` marks an unknown outcome |

A client treats a creation as made **only** on `phase == "ready"`. A
`CreationStatus` never contains `ok`.

`failed` codes:

* `rejected` — the daemon answered with a refusal (reproduced generically).
* `unavailable` — proven not sent: the daemon dial failed, the dispatch marker
  could not be recorded, or the server stopped before sending.
* `auth.revoked` / `auth.expired` / `denied` — access ended while queued; did
  not run.
* `not_found` / `archived` / `unsupported_verb` / `not_in_catalog` — the
  re-check before dispatch refused it (scope, archive, capability, catalog or
  ceiling changed while queued); did not run.
* `idem_ambiguous` with `uncertain: true` — sent, outcome unknown: the
  transport failed after sending, the 10-minute bound or the grant's absolute
  expiry ended the wait, access was revoked mid-creation, the daemon answered
  success without an id, or the server restarted after sending. **The daemon
  may well have created it.** It never runs again under its key; check the
  session list before retrying with a new key.

## Identity and recovery

* One `(grant, idem_key)` is one creation, sync or async (`async` is not part
  of the body hash). Re-POSTing it async answers its status under the same
  `creation_id` — in flight, done, or ambiguous, including a creation first
  sent synchronously (it is given an id on first async use, durably). A sync
  re-POST behaves as before (`409 idem_in_progress`, replay, or
  `idem_ambiguous`). A changed body is `422 idem_conflict` in both modes. Apply
  is never called twice for a key.
* Client disconnect and access-token expiry never cancel an admitted job: it
  holds its own lease. Refresh and poll.
* Restart: an async record admitted but never dispatched reports `failed` /
  `unavailable` (it did not run); one dispatched reports `idem_ambiguous`,
  `uncertain`. Neither is re-run. (A synchronous record carries no dispatch
  marker and is ambiguous after a restart, as before.)

## Execution

* After the key is recorded the job waits for one of 4 daemon slots. Then,
  immediately before dispatch, it re-checks the grant (not revoked, inside
  absolute and idle lifetime, current identity generation, still holding
  `create`) and re-runs the full destination precheck against fresh inventory
  and catalog. It durably commits a dispatch marker, and only then calls the
  daemon.
* The daemon call is bounded by `min(10 min, grant absolute expiry)` and ends
  at once on revocation. Revoke drains running jobs like any lease. The cut
  only stops the wait: a creation already sent may still complete on the
  host, which is why it reports `uncertain`.
* Bounds: ≤4 pending per grant, ≤32 per server, ≤4 concurrent daemon calls;
  reservations are released on every exit path. Records share the grant's
  existing `maxIdemPerGrant` / 24h retention.

## Known limit

The daemon has no creation request id, so a creation that outlives the bound
(or whose server restarted after sending) cannot be reconciled to the session
it produced; it stays `uncertain` rather than `ready`. Closing that needs a
daemon-side creation id and is tracked as a follow-up.
