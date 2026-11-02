# Direct asynchronous creation (design + acceptance matrix)

Status: proposed — awaiting coordinator sign-off before protocol code lands.

## Problem

A direct creation (`add-agent`, `new-workgroup`, `new-workgroup-empty`) runs
synchronously under `actionExecLimit` (60s). A slow repo fetch or worktree
preparation outlives it: `hostbackend.Join` closes the daemon socket and the
key is recorded `idem_ambiguous`. The daemon does **not** stop — it runs
`handle` under its own lifetime (`serveAuthenticated`'s `clientCtx`), so the
creation usually succeeds while the client was told "unknown". Harness relay
solves the same problem with `async: true`, a `creation_id` and
`daemon.create_status` (queued → preparing → ready | failed, `uncertain`).

## Wire (additive, `amux-direct/1`)

* `ActionRequest.async` (optional bool). Accepted only for the creation verbs;
  any other verb with it is `400 bad_request`. It is excluded from the body
  hash: the same `idem_key` names the same job whether sent sync or async.
* Every synchronous precheck is unchanged and still answers synchronously with
  no record: admission (`create`), root-creation scope, strict decode, scope,
  console, archive, catalog/ceiling (`not_in_catalog`).
* After the key is durably recorded the server waits up to 2s, then answers
  `202` (non-terminal) or `200` (terminal) with a `CreationStatus`:

  ```json
  {"creation_id":"crt_…","verb":"add-agent","phase":"queued|preparing|ready|failed",
   "new_id":"…","root_id":"…","code":"…","error":"…","uncertain":true,"elapsed_ms":1234}
  ```

* `GET /direct/v1/creations/{creation_id}[?wait=N]` — permission `create`,
  looked up only in the caller's grant (anything else is `404 not_found`);
  `wait` (≤20s, bounded by token expiry) long-polls until the phase changes.
* `CapabilitiesResponse.async_create` (omitempty) is true when the grant holds
  `create`. Clients that never send `async` see unchanged behavior and bytes.

## Semantics

| Situation | Phase / code | uncertain |
|---|---|---|
| admitted, waiting for an execution slot | queued | – |
| `dispatchedAt` durably committed, `Apply` running | preparing | – |
| `Apply` returned a valid id | ready (`new_id`, `root_id`) | – |
| daemon `RejectedError` | failed / `rejected` | false |
| `ErrUnavailable` before dispatch, or dispatch commit failed | failed / `unavailable` | false |
| 10 min bound, transport loss after dispatch, success without id | failed / `idem_ambiguous` | true |
| revoked while queued | failed / `auth.revoked` (not run) | false |
| revoked while preparing | failed / `idem_ambiguous` | true |
| server restart, record dispatched | failed / `idem_ambiguous` | true |
| server restart, record never dispatched | failed / `unavailable` | false |

Nothing with an unknown outcome is ever run again under its key. Client
disconnect and access-token expiry never cancel a job (it holds its own lease,
cut short only by revocation; revoke drains it like any lease).

Bounds: ≤4 pending async jobs per grant, ≤32 server-wide, ≤4 concurrent daemon
calls; beyond, `429 rate_limited` before anything is recorded. Records stay
within `maxIdemPerGrant` and `idemRetention`.

Known limit: the daemon has no creation request id, so a creation that outlives
the 10 min bound cannot be reconciled to its session and stays `uncertain`.

## Acceptance matrix

1. Slow async new-workgroup (longer than the sync bound) → 202 → ready, `root_id == new_id`.
2. Async add-agent → ready, `root_id` = target.
3. Client disconnects after 202 → job completes → ready.
4. Same key: async re-POST same `creation_id`; sync re-POST `409 idem_in_progress`; after done, replay; `Apply` called once.
5. Same key, different body → `idem_conflict`.
6. Rejection → failed/`rejected`, not uncertain, replayed.
7. Daemon unreachable → failed/`unavailable`, not uncertain.
8. Bound expiry / post-dispatch transport error → `idem_ambiguous`, uncertain, never re-run.
9. Revoke while preparing → drained; old token `auth.revoked`; a new grant gets 404.
10. Restart: dispatched → uncertain; undispatched → `unavailable`; same-key retry never calls `Apply`.
11. Other grant's id → 404; no `create` → `denied`; workgroup-scoped new-workgroup → `denied`; catalog/archive/console → synchronous 4xx, no record.
12. `async` on a non-creation verb → 400.
13. Per-grant / global caps → 429, nothing recorded.
14. Long-poll returns on transition; `wait` capped.
15. Token expiry mid-job does not cancel it.
16. S1/workflow-v1 fixtures unchanged except additive `async_create`; old store JSON loads.
