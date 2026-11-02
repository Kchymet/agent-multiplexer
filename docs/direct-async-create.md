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

`failed` codes fall in three groups. `uncertain: false` does **not** mean
"no effects": only the first group proves the creation was never sent.

**Did not run — zero effects proven** (recorded only before the dispatch
marker; any of these after dispatch becomes `idem_ambiguous`). A client may
start over with a fresh `idem_key`:

* `unavailable` — the daemon dial failed, the dispatch marker could not be
  recorded, or the server stopped before sending.
* `auth.revoked` / `auth.expired` / `denied` — access ended while queued.
* `not_found` / `archived` / `unsupported_verb` / `not_in_catalog` — the
  re-check before dispatch refused it (scope, archive, capability, catalog or
  ceiling changed while queued).

**Known reply, effects may exist:**

* `rejected` — the daemon answered not-OK (reproduced generically). That is a
  known answer, **not known zero effects**: the daemon may have created the
  session and then failed to start it, or created a workgroup root or
  worktrees before failing. Never retry it automatically with a fresh
  `idem_key`; keep the key and check the machine's session list first.

**Unknown:**

* `idem_ambiguous` with `uncertain: true` — sent, outcome unknown: the
  transport failed after sending, the 10-minute bound or the grant's absolute
  expiry ended the wait, access was revoked mid-creation, the daemon answered
  success without an id, or the server restarted after sending. **The daemon
  may well have created it.** It never runs again under its key; check the
  session list before retrying with a new key.

**Errors on the POST itself** (either mode). These answer before anything is
recorded or sent, so nothing ran: `400 bad_request`, `403 denied`,
`404 not_found`, `409 archived`, `409 unsupported_verb`,
`422 not_in_catalog`, `429 rate_limited`, `409 capacity_exhausted`,
`503 unavailable`. Not proof of not-run: `422 rejected`, `409 idem_ambiguous`,
`409 idem_in_progress` (it is running), `422 idem_conflict` (the key already
names another request), and **any `401`**. A sync creation revoked after
dispatch answers `401 auth.revoked` with an unknown outcome, and an async POST
answers `401` when the reader's own token ended while the recorded creation
continues. Re-authenticate and re-POST the same key; the replay is
authoritative. After revocation the grant's records are gone, so no retry can
clear an earlier uncertainty — check the machine.

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
* Restart: on load, an async record admitted but never dispatched is durably
  settled as a terminal, known `unavailable` ("did not run"); status, async
  replay and sync replay (`503 unavailable`) all give that answer, and as a
  done record it is pruned after the 24h retention. One dispatched stays
  ambiguous (`idem_ambiguous`, `uncertain`), as does any synchronous admitted
  record (it carries no dispatch marker). Neither is re-run.
* Every answer that carries a `CreationStatus` — the POST (fresh or replay)
  and the GET — is withheld with the reader's auth code if its own access
  ended meanwhile; the creation itself is unaffected.

## Execution

* After the key is recorded the job waits for one of 4 daemon slots. Then,
  immediately before dispatch, it re-checks the grant (not revoked, inside
  absolute and idle lifetime, current identity generation, still holding
  `create`) and re-runs the full destination precheck against fresh inventory
  and catalog. It durably commits a dispatch marker, and only then calls the
  daemon.
* The job's lease follows the grant's lifetime while queued and while
  running: it ends at the idle deadline (`last refresh + idle TTL`, re-read
  continually, so only a real token refresh extends it; the job and status
  polls never do), at absolute expiry, on an identity reset, or at once on
  revocation. The daemon call is further bounded by 10 minutes. Revoke drains running jobs like any lease. The cut
  only stops the wait: a creation already sent may still complete on the
  host, which is why it reports `uncertain`.
* Bounds: ≤4 pending per grant, ≤32 per server, ≤4 concurrent daemon calls;
  reservations are released on every exit path. Records share the grant's
  existing `maxIdemPerGrant` / 24h retention.

## Known limits and follow-ups

The daemon has no creation request id, so a creation that outlives the bound
(or whose server restarted after sending) cannot be reconciled to the session
it produced; it stays `uncertain` rather than `ready`. Closing that needs a
daemon-side creation id and is tracked as a follow-up.

Status replay identity (binding a replayed status or `new_id` to the session
incarnation it named, e.g. after that session is deleted and an id reused) is a
separate follow-up shared with the synchronous replay path; this change adds no
schema for it.
