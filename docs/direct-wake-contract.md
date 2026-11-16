# Direct wake-ups — contract proposal **v3.4** (for review)

> Published as the docs-only review baseline (authorized: contract publication, not implementation). The eventual backend PR carries this file.

Owner: b9731f (backend: agent-multiplexer + harness).

**Status: proposed.**
- **History.**
  - v1 was not approved: coordinator771951 raised 7 findings; reviewer87711e confirmed them and added A–E.
  - v2 answered the coordinator's 7.
  - v3 answers both reviews, plus a source finding of my own on session renewal (§12).
- **Implementation is stopped** until explicit authorization.

Audited at agent-multiplexer `origin/master` and harness `origin/master` (2026-11-15). Baseline builds and targeted tests are green.

## 0. Summary

An Android client connected **directly** to a machine (`amux direct`, its own grant) may opt in, per direct backend, to **content-free cloud wake-ups**.

1. **Detection on the host.** The machine's direct server detects the same events the relay notifies on: `permission_request`, and `turn_end` for sessions watched per subscription on the machine.
2. **Durable outbox on the host.** It records each wake in its wake store (`wake.json`), under authority fenced in `grants.json` (§4.4), and queues an opaque `(route_id, wake_id)` in a leased, retriable outbox.
3. **Forwarding through the provider.** `amux provide` forwards the wake over its authenticated connection and settles it only on a terminal Harness answer.
4. **Delivery to one client.** Harness checks, from server state only, that the route was **activated by proof** through this very provider. It then sends to the devices of **exactly one client**, through its own delivery path, claiming dedupe only after FCM acceptance. The payload is routing-only.
5. **Direct fetch, actions and ack.** The phone fetches details directly with its own credential (paginated, filtered at fetch time, gap-marked), acts with the existing direct `permission` verb, and acks.

**Relay notifications are unchanged.** Harness suppresses nothing.

**Coexistence is decided on the phone, per event:**
- when a relay card and a direct item have the same producer tuple, they collapse, and **direct routing and actions win** if the direct fetch covers the tuple;
- otherwise the relay card stands with its existing semantics.

Zero duplicates are **not** promised: the bounds are in §6.4.

## 1. Source map (audited)

### Relay notification path (unchanged)
| Concern | Location |
|---|---|
| Kinds; **owner-wide** fan-out (`Notify` iterates all owner devices, `push.go` L207); envelope; dedupe ledger **claimed before Send and never released** (`push.go` L244–262) | harness `backend/push/push.go` |
| Strict owner resolution | `backend/push/owner.go` |
| Daemon ingest → push, only with provider `sessions` + `runtime-events`; live only | `backend/engine/provider/runtime_events.go` |
| Relay watch key `(owner, scope, session_id)` with no provider column | `backend/store/devices.go` L267–275; `migrations/0012_devices.sql` L54–62 |
| FCM transport, `ErrTokenGone` | `backend/push/fcm.go` |
| Beta identity. Android **session TTL 8 h**, and a new session row per session start (`api/client_sessions.go` L96–100, L340–341; `store.CreateUserSession`). `RevokeUserSession` revokes only the session (`user_identity.go` L409–412). It is called only by `DELETE /api/auth/session` (`api/client_sessions.go` `RevokeCurrentSession`), and renewal and re-login are both `POST /api/auth/sessions`, so a session delete does not prove sign-out intent. `ActivePushAuthority`; `RevokeUserClient`; `RevokeAllUserAuthority`; `lockActiveUserAuthority` | `backend/store/user_identity.go`, `beta_write_authority.go` |
| Provider `capabilities` **persist after disconnect**; only `status` changes (`store/providers.go` L200–247). A live advert is tracked per hub connection. | `backend/store/providers.go`, `engine/provider/hub.go` L286 |
| Backend catalog lists a direct backend **only with the enroll advert** (`FromCapabilities` requires `Issuance` + `direct-enroll-v1`) | `backend/directenroll/advert.go` L47; `api/backends.go` L48–52 |
| Beta export/delete/restore coverage | `backend/store/beta_data.go`, `beta_restore.go` |

### Direct / enrollment path (reused)
| Concern | Location |
|---|---|
| CORS preflight allows **GET, POST only** | amux `internal/direct/server.go` L136 |
| One lock for grants, leases and revoke; `commitLocked` rollback/ambiguous semantics | `internal/direct/authority.go` |
| **One** registry file, `writeAtomic` + rename; lenient decode (old binaries drop unknown fields) | `internal/direct/store.go` L166–180, L356 |
| Scope (`inScope`, `filtered`, console, archived) | `internal/direct/sessions.go` |
| `Backend.Snapshot`, `History` (`Head`), `Stream(after)`; **`SessionIdentities`** (store incarnations, as used by creation replay) | `internal/direct/sessions.go` L25–45; `creation_identity.go` |
| Admin ops, the provider bridge | `internal/direct/admin.go`, `relay.go` |
| Cloud pin resolver from the live registration | `internal/direct/cloud.go`; `cmd/amux/direct_cloud_pins.go` |
| Provider advert, 30 s recheck, re-register; `registered.owner` | `internal/provider/directenroll.go`, `provider.go` |
| Event vocabulary | `harnessproto/events.go` |
| harnessproto pin in harness | `backend/go.mod` `v0.2.1-0.20261102033644-a3594e0e4d38` |

## 2. Event inventory: no new alert types

| Relay today | Direct wake | Condition |
|---|---|---|
| daemon `permission_request` | `permission_request` | every live event with the exact `item_id` + `request_id` + `runtime_generation` tuple |
| watched `turn_end` | `turn_end` | only for a session (and incarnation) watched **by that subscription** on the machine. `device_watches` is not used. |
| cloud `approval` | — | out of scope |

Not wake-worthy:
- events at or below the subscription's baseline;
- synthetic reconcile turn ends;
- inventory state changes.

The cloud payload has no kind.

## 3. Identities, binding and logging

| Identity | Issued by | Secret? | Use |
|---|---|---|---|
| `user_id`, `client_id` (android), `authorization_generation` | Harness | no | route owner, recipient, fences |
| `provider_id` + live owner | Harness | no | route ↔ machine (resolved by Harness, §6.1) |
| `server_id` | direct server | no | route ↔ direct server. Phone-local and in Harness only; **not in FCM** |
| direct grant `client_id` | direct server | no (public metadata, not a credential); not sent to Harness | subscription owner |
| `grant_ref` = b64url(HMAC-SHA256(policy key, grant client_id))[:22] | direct server | no; opaque, rotates per policy nonce | binds an active route to one grant |
| `route_id` = `dwr_`+b64url(32) | Harness | **no**: it appears in FCM and in direct request bodies. **Possession grants nothing.** | routing handle |
| `activation_code` = `dwa_`+b64url(32) | Harness | **yes**: returned once to the registering client; only SHA-256 stored; single use; 10-minute TTL; never logged, never in a URL | authenticates the route ↔ grant binding |
| `wake_id` = `dwk_`+b64url(16) | direct server | no; **unordered** | stable outbox/dedupe id |

**Binding proof chain.** Each link is checked from server-side state.

1. **U owns P.** The live provider row has `owner_user_id = U`. Sharing and admin are ignored.
2. **P is live with `direct-wake-v1`.** A **live hub connection** of P negotiated the feature (echoed) with `directWake.serverId = S`. Stored capabilities are never used: they persist offline.
3. **The host pins U and P.** The host's wake policy pins them, defaulted from the live registration.
4. **Route R = (U, C, P, S) was created by Android client C** under `lockActiveUserAuthority(U, g)`.
   - R is bound to C and g, **not** to a single Harness session. Sessions are re-created every ≤ 8 h, so session binding would kill routes in normal use.
   - Only C receives `activation_code`.
5. **Activation authenticates the binding, so it is not first-claim.** C sends `{route_id, activation_code}` to the direct server **with grant G's bearer token**. The host relays `{route_id, activation_code, grant_ref, requester?}` through P.
   - Harness checks the code hash, TTL and single use, `conn.providerID = R.provider_id`, and `S`. It then consumes the code and sets R `active` with `grant_ref`.
   - Without the code, no grant can bind R. **Squatting a route needs the secret.**
6. **Brokered grants get an extra check.** For a cloud-brokered G, `requester` = `G.CloudRequester` must equal U.

**Rotation and reauthorization.**
- A new direct grant on the same server requires a **new route**: register again, which revokes the old active route for the tuple (`replaced`), mints a new code, and activates with the new grant.
- The old grant's subscription is deleted when it is retired.
- Route sync (§4.6) deactivates any route whose subscription is gone.
- A `grant_ref` mismatch on any wake for R is `route_gone`, with no mutation.

**Logging rule** (both repos):
- route_id and wake_id are logged only as an 8-character SHA-256 prefix;
- activation codes, grant client ids and session ids are never logged on the wake path;
- route_id never appears in a direct URL (request bodies only).

## 4. Machine side (agent-multiplexer)

### 4.1 Opt-in (off by default)

- **Host policy:** admin ops `wake_enable`, `wake_disable` and `wake_status` (SubjectHost). CLI: `amux direct wake enable [--owner --provider]`, `disable`, `status [--json]`.
  - State lives in `storeState.Wake.Policy {enabled, nonce, owner, provider, changed_at, key}`.
  - `nonce` is 16 random bytes, new on every change. It is not a counter, so an old-binary drop followed by re-enable can never collide with earlier records.
  - `key` is a fresh 32-byte HMAC key per nonce.
  - Disable, re-pin and `reset-identity` commit the authority fence (policy nonce, subscriptions) in `grants.json` first. The wake data then becomes inert and is cleaned up best-effort (§4.4).
- **Provider opt-in:** `amux provide --direct-wake` (`AMUX_PROVIDER_DIRECT_WAKE=1` / `direct-wake = true`). It advertises only while the policy is enabled with matching pins; same 30 s recheck.
- **No other provider features are needed.** `sessions`, `runtime-events` and `direct-enroll-v1` are **not** required. Wake-only discovery and registration work without them (§6.1, tested in §8).

### 4.2 Direct API (GET/POST only, so current CORS is unchanged; bearer access token; `read`; no new permission)

JSON bodies. Unknown fields and query parameters answer `400`. route_id is never in a path or query. All grant kinds opt in without re-enrollment.

| Route | Body | Result |
|---|---|---|
| `POST /direct/v1/wake/subscribe` | `{route_id, activation_code}` | This grant's subscription + an `activate` outbox item, in one commit. `200 {route_id, state:"activating"\|"active"\|"failed", failure?, created_at}`. Same grant + route + code is idempotent.
  - **No global reservation (v3.2).** Subscriptions are keyed by (grant, route). Several grants may hold *pending* subscriptions for the same route, and none blocks another. Only the one whose proof Harness accepts becomes `active`. The others settle `failed:route_gone`, and a wrong code never burns the real one (§6.2).
  - A host whose route is already `active` for grant A still accepts B's subscribe; B's activation is refused by Harness without mutation.
  - `409 wake_disabled`. `409 capacity_exhausted` (more than 4 per grant). |
| `POST /direct/v1/wake/unsubscribe` | `{route_id}` | `200 {removed}`. Idempotent. Deletion + `deactivate` item in one commit. |
| `POST /direct/v1/wake/subscriptions` | `{}` | this grant's `[{route_id, state, failure?, created_at, watches:[{session_id}]}]`. POST, so route ids never sit in a GET URL. |
| `POST /direct/v1/wake/watch` | `{route_id, session_id, incarnation, watch:bool}` | Adding requires the session in scope now (`404 not_found`) and `incarnation` equal to the current one (`409 incarnation_changed` returns the current incarnation). Removing never needs access. At most 64. Response: `{route_id, session_id, incarnation, watched}`, with the **current** incarnation. |
| `POST /direct/v1/wake/events` | `{route_id, after_seq?, limit:1..64}` | §4.4 |
| `POST /direct/v1/wake/ack` | `{route_id, wake_ids:[≤64]}` | `200 {acked}`. Idempotent. |

`GET /direct/v1/capabilities` gains `wake:{v:1, enabled:bool, degraded:bool}`, always serialized by new servers. Old servers answer a 404 with no JSON code. A CORS preflight test covers each new route.

### 4.3 Detection: baseline, monotonic high-water marks, incarnations

- **Session set.** The union of in-scope sessions over subscriptions: valid id, not console, not effectively archived.
- **Per-subscription marks (v3.2).** There is no shared mark that a new subscriber could advance past another subscriber's backlog.
  - **What is stored.** Each (subscription, session) pair has a durable high-water mark `mark[sub][sid]`, which **only goes up**, plus `incarnation[sid]`.
  - **New subscriber.** When a session enters a subscription's set, one commit sets **only that subscription's** `mark[sub][sid] = Head` (from `History`). Every other subscription's mark is untouched, so its pending backlog is still processed. `incarnation[sid]` is recorded if this is the first observer.
  - **Read position.** The detector reads each session from `min over subs of mark[sub][sid]`. This is derived, not stored.
  - **Processing an event.** For each event with ordinal `o`, every subscription with `mark[sub][sid] < o` evaluates it. The resulting records and all those subscriptions' new marks commit **atomically in one commit**. No subscription's mark advances without its records.
- **Mark behavior.**
  - Tails resume from the minimum mark: on idle → live, on restart, and on tail reconnect. A fast idle → permission inside one poll interval is therefore caught.
  - **Dedupe rests on the marks.** An event at or below a subscription's mark is never evaluated again for that subscription, so ack or expiry cannot re-queue it. Identity tombstones (24 h) are defense in depth only.
  - **Required test:** a new subscription added while an existing subscription has an unprocessed backlog. The old subscription still gets its wakes, and the new one gets none for events at or below its baseline.
- **Incarnation.** If `SessionIdentities` reports a different incarnation for `sid`, or the session is gone, or `Head` < its minimum mark:
  - every subscription's mark for that session is reset to the new head, in one commit;
  - watches recorded for the old incarnation are **deleted**, so a reused id never inherits a watch;
  - open permission records of the old incarnation become `stale`.
- **Identities.**
  - A permission's identity is `sha256(sid, incarnation, "permission_request", item_id, request_id, runtime_generation)`, length-prefixed.
  - A turn end's identity is `sha256(sid, incarnation, "turn_end", ordinal)`.
- **Permission state.**
  - `open` → `resolved` when a matching `permission_resolved` (same request_id + runtime_generation) is seen.
  - `open` → `stale` when a later event in the session carries a different non-empty `runtime_generation`, or the session is re-baselined (incarnation change).
  - **v3.4:** the "session goes idle" trigger is removed. Evidence that a session waiting on a permission is never reported idle, for both Claude and Codex, is not established (review P1), so idleness never stales a permission.
  - **Re-baseline on `Head` < mark** is a defensive trigger. Its occurrence is not claimed (P2). The primary trigger is the incarnation change, and both are tested with a scripted backend.
  - Answering is still validated by the runtime.
- **No starvation.** Up to 64 live sessions are tailed with `Stream(mark)`.
  - All others are polled round-robin with `History`, paging back to the mark (≤ 8 pages per turn; continued next turn, never skipping).
  - Each in-set session is checked at least every `max(2 s, ⌈N/16⌉×2 s)`.
  - If any session's lag exceeds 60 s, `degraded:true` appears in `wake status` and capabilities.

### 4.4 Durability: authority in `grants.json`, wake data in a separate file

**Decision (v3.4, coordinator after review R3).** v3's single-file choice is **replaced**. Putting high-frequency wake writes in `grants.json` couples an optional transport to core direct availability: an ambiguous commit blocks all token issuance and actions (`internal/direct/authority.go` `commitLocked`/`issuableLocked`, `degraded`).

- **Authority, in `grants.json`, under the existing commit semantics.**
  - `storeState.WakePolicy {enabled, nonce (16 random bytes), owner, provider, key, changed_at}`.
  - `grantRecord.WakeSubs[route_id] = {created_at, nonce, state: activating|active|failed, failure?, activation_digest}`.
  - These change only on low-frequency authority actions: enable, disable, re-pin, subscribe, unsubscribe, activation settle, grant revoke or expiry, and `reset-identity`.
  - **Revoke, disable and re-pin commit this fence first.** Wake data cleanup follows, best-effort.
- **Wake data, in `wake.json`** (mode 0600, sibling, `writeAtomic`, its own lock and its own store object).
  - It holds marks, baselines, incarnations, watches, records, tombstones and outbox state.
  - Each entry is keyed by `(grant client_id, route_id, policy nonce)`.
  - Writes are **batched**: at most one fsync per 250 ms, plus one immediately after a settle.
  - **No two-file atomicity is claimed.**
- **Every wake operation re-checks authority.** Detection, record creation, lease, settle, fetch, ack and watch each confirm, under a short read of the authority:
  - the grant is live;
  - the policy is enabled with the same nonce;
  - the `WakeSubs` entry exists, and is `active` (for lease of a wake item; activate items need `activating`).

  A wake entry that fails the check is **inert** and is deleted lazily.
- **Wake-file failures never touch core authority.**
  - A rolled-back `wake.json` write reverts in memory.
  - An **ambiguous** `wake.json` write sets **wake-degraded** only: wake ops pause and `wake status` reports it. A restart reconciles. It never sets the authority's `degraded`.
  - Direct issuance and actions are unaffected.
- **Startup reconciliation.** Load `wake.json`, then drop every entry whose grant is absent, revoked or expired, whose nonce ≠ current, or whose `WakeSubs` entry is gone. The cleaned file is written only if something changed.
- **Old-binary rewrite.** It drops `WakePolicy` and `WakeSubs`, so wake is **off** and `wake.json` is fully inert (no nonce matches). Re-enable mints a new nonce, so nothing earlier can match.
- **Locks.** The wake lock is never held while taking the authority lock for a write. Authority checks are a brief read under the authority lock, done before the wake lock is taken, and the wake operation then re-checks the nonce. Wake writes never run under the authority lock.
- **Required failure-order tests.**
  1. Revoke commits in `grants.json` while the `wake.json` delete fails → no lease, fetch or wake for that grant.
  2. Disable commits, then a crash before the `wake.json` cleanup → after restart, reconciliation drops all entries.
  3. Re-pin → old-nonce entries inert.
  4. An ambiguous `wake.json` write → wake-degraded, while direct tokens, actions and history keep working.
  5. An ambiguous `grants.json` commit during subscribe → the existing authority degradation, unchanged.
  6. Old-binary rewrite of `grants.json`, then re-enable → no stale match.
  7. A crash between record creation (`wake.json`) and the mark advance → the event is re-evaluated, and identity tombstones dedupe it.

**Fetch.** `POST /direct/v1/wake/events {route_id, after_seq, limit}` returns:

```json
{"route_id":"dwr_…","items":[
  {"seq":41,"wake_id":"dwk_…","kind":"permission_request","state":"open",
   "session_id":"…","incarnation":"…","item_id":"…","request_id":"…","runtime_generation":"…","ordinal":123,"created_at":…},
  {"seq":42,"wake_id":"dwk_…","kind":"turn_end","state":"done","session_id":"…","incarnation":"…","ordinal":130,"created_at":…}],
 "next_seq":42,"has_more":false,"gap":{"through_seq":37,"reason":"cap"}}
```

- **Ordering and paging.** `seq` is per-subscription and monotonic: the only order and cursor (exclusive). `wake_id` is unordered.
- **Gap marker.** `gap` is present when records with `seq > after_seq` were dropped (cap of 128, 24 h expiry, fetch filtering of now-invisible items). It tells the phone to do a full direct read (open permissions from `history`) instead of trusting the list.
- **Filtered when fetched.** An item is returned only if all of these hold now:
  - the grant is live with `read`;
  - the policy nonce is current;
  - the session is in scope and not console;
  - the incarnation is current.
  Items that fail are tombstoned in the wake store.
- **Unknown routes.** An unknown route, or another grant's, returns `200 {"items":[]}`.
- **Content and actions.**
  - **Content** comes only from `history`/`stream`.
  - **Answering** uses the existing `permission` action, which requires `approve`.
  - **Allow/Deny** is shown only for `open` items when capabilities `verbs` has `permission`.
- **Ack.** Ack tombstones records.

### 4.5 Leased, retriable outbox (no pre-send loss)

Item kinds: `wake`, `activate`, `deactivate`. Durable states: `pending | delivered | gone | expired`.

- **Activation first (R1).** A subscription's `wake` items are leasable **only after** its `activate` item settled `accepted`, so `WakeSubs.state = active`. Until then they wait, and their 900 s deadline still runs. Harness also answers a wake for a **pending** route with **`unavailable`** (retryable), never `route_gone`, as defense in depth.

- **Leasing.** `wake_lease {max≤16}` is a long-poll under 15 s. It gives `lease_id` + items. Leases are in memory only, 30 s.
- **Settling.** `wake_settle {lease_id, results}` is called **after** the Harness answer.
  - `accepted` / `no_recipient` → `delivered`.
  - `route_gone` → `gone`, subscription `failed:route_gone`.
  - `rate_limited` / `unavailable` / `busy`, a frame timeout or a disconnect → back to `pending`, with backoff from 2 s to 60 s.
- **At-least-once, deduped. Not exactly-once.** A re-send carries the same `wake_id`, and Harness's delivery ledger (§6.3) suppresses re-sends it already recorded as accepted. A crash after FCM accepted but before the ledger write can still duplicate.
- **Deadlines.**
  - A wake stops retrying 900 s after creation (`expired`); the record stays fetchable.
  - `activate` expires with the code TTL (10 min) → `failed:activation_expired`.
  - `deactivate` retries up to 24 h; route sync converges it anyway.
- **Revocation race.** Revoke, unsubscribe or a policy change commits the authority fence first (§4.4). Items become inert immediately and are deleted afterwards. A later settle for a missing item is ignored. At most one wake that was already submitted can be delivered; its fetch answers 401 or is empty.

### 4.6 Route sync

On every registration and every 30 s, the provider sends `direct-wake-routes {serverId, epoch, routes:[{routeId, grantRef, state:"activating"|"active"}], part, final}` (≤ 512 per part).

- **What a sync lists (R2).** Every `activating` **and** `active` subscription. That covers every route a grant on this host has authenticated a subscribe for.
- **Epochs.** `epoch` is a random id per snapshot. Harness applies a sync **only after it holds every part of one epoch** (parts 0..n with `final` on n, same epoch, same connection). Mixed or incomplete epochs are discarded.
- **Scope.** Harness applies it **only to routes with `provider_id = conn.providerID` and `server_id = serverId`**.
- **Misses.** An active route absent from two consecutive complete epochs → revoked (`deactivated`). Misses count only for routes activated **more than 60 s** before the epoch was received, which is the grace period.
- A `grantRef` mismatch → ignored for that route.
- Unknown or foreign ids → ignored, with no mutation.

## 5. Wire

### 5.1 harnessproto `direct-wake-v1` (additive; goldens)

| Frame | Direction | Body |
|---|---|---|
| `registered.features` | orch → prov | accepted features echoed. Wake frames are sent only after the echo. |
| `direct-wake-activate` | prov → orch | `{reqId, directWake:{serverId, routeId, activationCode, grantRef, requester?}}` |
| `direct-wake` | prov → orch | `{reqId, directWake:{serverId, wakes:[{routeId, wakeId, grantRef}] ≤16}}` |
| `direct-wake-routes` | prov → orch | `{reqId, directWake:{serverId, epoch, routes:[{routeId, grantRef, state}] ≤512, part, final}}` |
| `direct-wake-result` | orch → prov | one per reqId, within 15 s: `{reqId, directWakeResult:{results:[{routeId, wakeId?, status}], error?}}` |

- **Statuses:** `accepted`, `no_recipient`, `route_gone`, `rate_limited`, `unavailable`, `busy`.
- **Connection-level problems** (the serverId is not the connection's current negotiated one; a frame during re-registration) answer `error:"unavailable"` for the whole frame. **They never answer `route_gone`**, so a transient advert flip cannot drop subscriptions.
- At most 4 frames in flight.
- `activationCode` is the only secret on the wire.
- **Compatibility.**
  - Old Harness: no echo, so nothing is sent; subscriptions become `failed:activation_expired`.
  - Old provider: no advert, so registration answers `wake_unavailable`.
  - Old direct server: 404 without a code.

### 5.2 FCM data payload (data-only, `priority: high`, `ttl: 900s`)

```jsonc
{ "kind":"direct_wake", "v":"1", "route_id":"dwr_…", "wake_id":"dwk_…", "ts":"RFC3339",
  "harness_client_id":"…","user_id":"…","origin":"…","authorization_generation":"…" }
```

- **No `server_id`** (finding E): the phone maps route_id → backend locally.
- No kind, session, request, generation or item, and **no title or body**. The phone composes any display only after a direct fetch.
- **Renewal-safe authority envelope** (§13.4). The wake envelope carries only `user_id`, `harness_client_id`, `origin` and `authorization_generation`, which the phone exact-matches against its current Harness authority.
  - The session-bound `harness_session_id` and `expires_at` are **omitted**: sessions are re-created at least every 8 h, and a wake queued across a renewal must still pass.
  - The relay payload's envelope is unchanged.

## 6. Harness side

### 6.1 Discovery and routes (migration `0022_direct_wake_routes.sql`; added to beta export/delete/restore lists)

```sql
CREATE TABLE direct_wake_routes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES app_users(id),
  client_id TEXT NOT NULL REFERENCES user_clients(id),
  provider_id TEXT NOT NULL, server_id TEXT NOT NULL,
  authorization_generation BIGINT NOT NULL,
  activation_hash TEXT, activation_expires_at TIMESTAMPTZ,  -- NULL once consumed
  grant_ref TEXT, activated_at TIMESTAMPTZ, sync_misses INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(), revoked_at TIMESTAMPTZ, revoke_reason TEXT);
CREATE UNIQUE INDEX direct_wake_routes_active ON direct_wake_routes(user_id,client_id,provider_id,server_id) WHERE revoked_at IS NULL;
```

**Wake-only discovery, independent of the enroll catalog.**
- The phone already holds the verified `server_id` of each direct backend, manual or brokered.
- Harness keeps an in-memory index of **live** hub connections that negotiated `direct-wake-v1` (provider → serverId), maintained on register and disconnect.
- **`device.direct_wake.targets`** returns `[{server_id, available:true}]` for the caller's owned providers that are live with wake. It lets the phone show "available" for backends whose server_id matches. It does not need `direct-enroll-v1` or `sessions`.

**Verbs.** They require a beta Android principal. Legacy, email-only and browser identities are refused explicitly: no fallback.

- **`device.direct_wake.register`** (`server_id`, `request_id`; there is **no provider_id input**: Harness resolves it and returns it).
  - **Provider resolution.** Harness resolves the provider from the live index: exactly one owned provider live with `directWake.serverId == server_id`. None answers `wake_unavailable`; more than one answers `wake_ambiguous`.
  - **Checks:**
    - the caller owns that provider (live row);
    - at least one device row exists for (U, C);
    - the write runs under `lockActiveUserAuthority`;
    - at most 8 active routes per client.
  - **Request id.** The client sends a fresh `request_id` (b64url of 16+ random bytes) per setup attempt.
  - **New request id.** An existing route for (U, C, P, S), pending or active, is revoked (`replaced`) and a new one minted. The route records the `request_id`.
  - **Replay of the same `request_id`** (the phone lost the reply):
    - while that route is still **pending**, the same `route_id` is returned with a **new** code (the stored hash is replaced). No activation has happened, so nothing newer is undone;
    - while that route is **active**, the answer is `{route_id, provider_id, state:"active"}` with no code;
    - if that route was **replaced or revoked** by a newer request, the answer is `409 superseded`. A stale retry can never revoke or undo a newer setup.
  - **Result:** `{route_id, provider_id, activation_code, expires_at}`.
- **`device.direct_wake.unregister`** (`route_id`): owner- and client-scoped, idempotent.
- **`device.direct_wake.sign_out`** (`reason: "sign_out"|"server_change"`; an absent reason is treated as `sign_out`, the safe default): revokes **all** of the caller client's routes, recording `revoke_reason` = the reason. It is idempotent and needs a live session.
  - **Order on the phone (R4).** Android first drops its route mappings and sets the local fence (§14), then calls this verb, then `DELETE /api/auth/session`.
  - **Interactive re-sign-in** (including the same account, `AuthRepository.kt` L242–251) does **not** call this verb, so wake routes survive it.
  - **`DELETE /api/auth/session` is unchanged and never revokes routes.**
- **`device.direct_wake.list`**: the caller client's routes `{route_id, server_id, provider_id, state:"pending"|"active", created_at}`. Codes are never returned.

### 6.2 Validating provider input: no cross-tenant mutation

The provider is always `conn.providerID`. Answers for foreign and nonexistent routes are **indistinguishable**.

| Condition | Answer | Mutation |
|---|---|---|
| Frame `serverId` ≠ the connection's current negotiated serverId, or the connection is not in the live index | frame `error:"unavailable"` | none |
| Route unknown, or `provider_id ≠ conn.providerID`, or `server_id ≠ serverId` | `route_gone` | **none** |
| Route pending (wake) | **`unavailable`** (retryable, R1) | none |
| `grantRef ≠ route.grant_ref` | `route_gone` | none |
| The route's own binding is invalid from server state: provider owner ≠ `route.user_id`; user inactive; generation ≠ route's; client revoked | `route_gone` | revoke **this** route. Allowed because it belongs to `conn.providerID` and the reason is server state, never frame data. |
| Activation: hash mismatch | `route_gone` | none (a forged code never burns the real one) |
| Activation: hash match, route already active with the **same** `grant_ref` and provider (lost-ack retry) | **`accepted`**, idempotent | none. The proof hash is kept after consumption for exactly this check. |
| Activation: hash match but the route is active with a **different** `grant_ref`, or the code expired before first use | `route_gone` | none |
| Activation: requester ≠ `route.user_id` | `route_gone` | consume and revoke (`requester_mismatch`) |
| Rate: 30/min per route, 120/min per provider | `rate_limited` | — |
| Transient DB error | `unavailable` | — |

### 6.3 Delivery: one client; accepted state recorded only after FCM acceptance

- **A new `push.DirectWakeDelivery`**, separate from relay `Notify` (finding A).
  - It reuses only the `Sender` interface and the authority-envelope builder.
  - Recipients are `devices WHERE owner_user_id = route.user_id AND client_id = route.client_id`, and nobody else.
- **The send-boundary query.** One SQL statement, run immediately before each `Send`, returns the device token together with the envelope. It joins:
  - `direct_wake_routes` (active, not revoked);
  - `devices` (owner + client match);
  - `user_clients` (not revoked, kind android);
  - `app_users` (active, `authorization_generation = route.authorization_generation`);
  It does **not** require a live `user_sessions` row: route-bound wake authority (§14) is the notification registration lifetime, not the cloud session. The relay `PushGate` and `ActivePushAuthority` for content and actions are unchanged.
- **Delivery ledger (v3.2).** This is its own table, `direct_wake_deliveries(device_id, wake_key, state, lease_until, accepted_at)`, and the relay `device_push_dedupe` is untouched. `wake_key` = `sha256(provider_id, server_id, wake_id)`.
  1. **Lease.** Upsert to `state=pending, lease_until=now+30s`, except where `state=accepted` (→ skip the device; it is already delivered) or `state=pending` with a live lease (→ the device result is **`busy`**, never treated as delivered). An **expired** pending lease is taken over: crash before send ⇒ retriable.
  2. `Send`.
  3. **On success,** `state=accepted, accepted_at=now`, durably.
  4. **On a transient error** (429, 5xx, timeout), the row is deleted, giving `unavailable`.
  5. **On `ErrTokenGone`,** the token is pruned (as today) and the row deleted.
  6. **Crash after send, before step 3.** The lease expires and the wake is re-sent, a **possible duplicate**. The contract claims at-least-once, never exactly-once.
- **Overall result.**
  - `accepted` if every recipient device is `accepted`.
  - `busy` if any device holds a live pending lease from a concurrent attempt. That is retriable and is never a terminal acknowledgment.
  - `unavailable` if any device had a transient failure. The retry re-sends only to devices not yet accepted.
  - `no_recipient` if the client has no device row. This is terminal for that wake; the record stays fetchable.
- **The relay `Notify` ledger behavior is unchanged.** It is out of scope, and the review noted it as pre-existing.

### 6.4 Coexistence: phone-side, per event; Harness suppresses nothing

- **Relay behavior is unchanged.** The relay keeps pushing relay cards (all owner devices, existing `device.watch`). Direct wakes are delivered independently to the opted-in client. **Harness never suppresses either path**: it cannot know grant scope.
- **Exact permission correlation (phone).** A relay card and a direct item correlate **only if every one of these holds**. Otherwise they are independent cards.
  1. **Verified provider ↔ server pairing.** The relay push's `scope=daemon`, `id` (provider_id) equals the provider_id of the phone's **active** route for this direct backend (`device.direct_wake.list`).
     - Harness resolved that provider from a live connection advertising this `server_id` (§6.1).
     - The phone verified that same `server_id` through the direct signed hello at enrollment.
     - Both halves of the pairing are therefore verified, not just raw ids.
  2. **Common session incarnation, verified rather than inferred.** Both sides carry the **same** daemon store incarnation (`internal/store/identity.go` `SessionIdentity.Incarnation`), and they must be equal and non-empty:
     - **Direct side:** the item's `incarnation`. Fetch returns only current-incarnation items (§4.4).
     - **Relay side:** a new additive push field `session_incarnation`. Harness copies it from the provider's latest `sessions` inventory row for that session (§13.6), which gains `incarnation`.
     - A relay push without `session_incarnation` (older provider or Harness) **never correlates**: both cards are shown.
     - Runtime tuple uniqueness is not assumed.
  3. **Equal producer tuple.** `session_id`, `item_id`, `request_id` and `runtime_generation` are equal. None of them may be empty: a generation-free relay row never correlates.
- **When a correlated item is covered by direct.** "Covered" means the direct fetch returned the item, in scope and `open`.
  - The card is **direct-owned**: content from direct, actions via the direct `permission` verb only.
  - **An opted-in direct card never routes an action via relay.**
  - **A grant without `approve` gets a view-only direct card.** It shows no Allow/Deny, and the correlated relay card is collapsed into it. The phone does **not** quietly fall back to relay actions for a covered item. (Android decision, coordinator review.)
- **Out-of-scope and uncorrelated cloud cards are never dropped.** A relay card is replaced only by a direct card that covers the same exact tuple. In every other case the relay card stands with its existing semantics:
  - the session is outside the grant's scope;
  - the direct fetch failed or timed out;
  - there is no active route;
  - the pairing or incarnation does not match, or the relay push lacks `session_incarnation`.
- **Timing (stated bounds, no zero-duplicate promise).**
  - If the relay card is drawn first, the phone replaces it under the same collapse key once the direct fetch covers the tuple.
  - If the direct card is drawn first, a later relay push for a covered key is dropped.
  - A duplicate can remain visible for at most one direct fetch round-trip, or permanently only if the phone lacks the provider ↔ server mapping.
- **turn_end: exact correlation only, no fuzzy windows.**
  - The relay daemon `turn_end` push gains the additive fields `ordinal` (the ingested row's daemon ordinal) and `session_incarnation`.
  - A relay turn-end card and a direct turn-end item correlate only on the verified provider ↔ server pairing + `session_id` + `session_incarnation` + `ordinal`, all equal and non-empty.
  - A relay push without these fields **never** correlates, so both cards are shown. Collapsing by provider, session or time window is **forbidden**, because it would merge distinct turns.
  - A session watched only on the direct side always gets its turn end.
- **Scoped grants.** Direct wakes cover only the grant's scope and its own watches. Relay coverage for everything else is unchanged.

### 6.5 Fences: revocation, logout, removal, rotation, late work

| Event | Effect | Late work |
|---|---|---|
| **Explicit Harness sign-out or server change** on client C | The phone calls `device.direct_wake.sign_out` **before** `DELETE /api/auth/session`, which revokes all of C's routes (`signed_out`), and it fences locally (§14). A generic `RevokeUserSession` is **not** hooked, because a session delete is not proof of sign-out intent. | route state is checked in the send-boundary query |
| Cloud session **expiry** (8 h; Android does not renew silently) | **No effect on wake delivery.** Route-bound wake authority does not depend on a live session (§14). Cloud content and actions still need a fresh sign-in, as today. | — |
| Client revoke, revoke-all, generation bump | routes revoked in the same transaction | generation stored per route; register runs under `lockActiveUserAuthority` |
| FCM token rotation | the new device row for (U, C) is used; routes are not token-bound | — |
| Android disables wake for the backend | direct `unsubscribe` (stops the source, queues `deactivate`) + `device.direct_wake.unregister`. Either one alone stops delivery. | — |
| Direct grant revoke or expiry, `reset-identity` | wake state deleted in the revoke commit; sync deactivates | at most one already-submitted wake; its fetch answers 401 |
| Direct reauthorization (new grant) | phone re-registers, so the old route is `replaced` and the old grant's subscription is deleted on retire | `grantRef` mismatch answers `route_gone` |
| Wake policy disable or re-pin | everything cleared in one commit; new nonce and key; advert withdrawn | — |
| Provider owner reassigned or recreated | advert withdrawn; binding check revokes that provider's routes | — |
| Provider disconnected or Harness down | outbox retries up to 900 s; records fetchable; the route stays active until two complete syncs miss it | — |

### 6.6 Cloud unavailable or disabled

- **Direct never depends on wake.**
- **The outbox absorbs outages** for 900 s; after that, records are still fetchable on the host.
- **No configured FCM sender** answers `no_recipient`.
- **Disabling wake never touches grants.**

## 7. Threat boundaries

| Actor | Can | Cannot |
|---|---|---|
| Harness / FCM | see route_id, wake_id, timing and the beta envelope (FCM); additionally server_id and provider_id (Harness) | see session ids, kinds, content, request ids, generations or direct tokens; fetch; answer |
| Another Harness user or provider | — | register or activate on providers it does not own; mutate a victim route (foreign input causes no mutation, and its answer is indistinguishable) |
| Holder of a route_id | — | activate (needs the code), subscribe-to-activate, fetch, revoke |
| A second direct grant on the same host | hold an inactive subscription that never activates | squat (needs the code), widen permissions, read other grants' records |
| U's own compromised provider | spam U's routes up to the rate limits; deactivate its own routes | touch other providers' routes; forge a brokered requester |
| Replay | — | double-buzz (claim-after-accept ledger), act |
| Lost phone after revocation | receive at most one in-flight wake | fetch it (401) |

## 8. Tests (acceptance)

**amux.**
- **Goldens** for the five frames plus `registered.features`; a negative content test.
- **Atomicity:**
  - fault-injected `saveState` (rolled-back and ambiguous) during revoke, disable, re-pin, reset, subscribe and settle → memory and disk consistent;
  - **old-binary rewrite then re-enable** → no earlier record, grant_ref or outbox item matches.
- **Detector:**
  - idle → permission inside one poll interval is woken;
  - restart behind the mark gives no duplicate of an acked record;
  - incarnation change deletes watches and stales permissions;
  - truncation re-baselines;
  - 100 sessions: no starvation, `degraded` reported past 60 s lag;
  - resolved and stale transitions.
- **Outbox:**
  - disconnect, timeout, `busy`, `rate_limited` and `unavailable` → re-delivered with the same wake_id, and accepted re-sends are suppressed (at-least-once; no exactly-once claim);
  - `route_gone` → failed;
  - 900 s expiry;
  - activation TTL;
  - settle after revoke is ignored.
- **API:**
  - auth, scope, no oracle, conflict, capacity, policy-off;
  - seq paging with unordered ids;
  - **gap marker**;
  - ack idempotency;
  - fetch filter;
  - **CORS preflight for every new route**;
  - route_id never accepted in a URL.
- **Provider:**
  - the frame is sent only after the echo;
  - route sync parts;
  - settles only after the Harness answer;
  - a connection-level `unavailable` never drops subscriptions.
- **Live e2e:**
  - real direct server + daemon + fake runtime permission;
  - provider over a pipe to a fake orchestrator: activate → wake → settle;
  - the phone client fetches, answers `permission` and acks;
  - **with `sessions`, `runtime-events` and `direct-enroll-v1` all disabled**.

**harness.**
- **Real Postgres:**
  - register under the generation lock;
  - provider resolution by server_id from the live index (none / ambiguous);
  - activation (hash, TTL, single use, a forged code does not burn the real one);
  - `device.direct_wake.sign_out` revokes all of the client's routes (**logout → login does not revive**);
  - cascades from `RevokeUserClient` and `RevokeAllUserAuthority`;
  - **`RevokeUserSession` alone does not touch routes**: a generic session delete is not treated as sign-out intent.
  - **session expiry keeps the route deliverable** (no `user_sessions` requirement); explicit `device.direct_wake.sign_out` revokes it;
  - the unique index;
  - beta export/delete.
- **Hub with real `providerConn` frames:**
  - echo;
  - every §6.2 row, including **foreign-provider frame with a victim route_id: no mutation and an identical answer**, and a stale serverId answered `unavailable`;
  - a route squat by a second grant fails without the code;
  - reauthorization rotation;
  - requester mismatch;
  - rate limits;
  - route-sync scoping;
  - wake-only registration with a provider advertising only `direct-wake-v1`.
- **Delivery:**
  - only client C's devices (another device of U is not notified);
  - FCM 429/5xx/timeout releases the claim, and the retry delivers once;
  - success keeps the claim;
  - `ErrTokenGone` prunes;
  - send-boundary recheck after a mid-flight `sign_out`, client revoke or generation bump;
  - payload key-set allowlist (no server_id);
  - no device → `no_recipient`; an expired session still delivers.
- **Coexistence:** relay tests are unchanged, and the relay ledger behavior is untouched. Relay on plus a direct wake for the same permission: both are delivered by the backend, and the payloads carry the collapse-key fields (relay: existing; direct: via fetch). Phone-side collapse is Android's acceptance.
- **FCM boundary:** a fake `push.Sender`. No real FCM call or device token. Disclosed.

## 9. Landing order and pins

1. **agent-multiplexer PR** (master-based).
2. **harness PR** (master-based). It pins harnessproto to the amux PR head for CI, re-pins to the merged amux master commit before it is marked mergeable, and merges after amux.
3. Android consumes the published contract. Phone-side collapse and gating are Android's.

## 10. Decided defaults

- Retention 24 h, cap 128, FCM TTL 900 s, with bounded loss, the gap marker and recovery documented.
- No coarse kind in the cloud payload.
- A live negotiated provider connection is required at route registration and activation.

## 11. Legacy defects, evidence only (no fix in scope)

- **The relay watch key has no provider column** (`device_watches`, `0012_devices.sql` L54–62, `devices.go` L267–275). The new path does not use it.
- **The relay `Notify` claims dedupe before `Send` and never releases it** (`push.go` L244–262), so an FCM 429/5xx loses that relay push.
  - **Reproduced, 2026-11-15.** A temporary test (since deleted, nothing committed) used `fakeRegistry` + `recordingSender`. The first `Notify` got a 503; after FCM recovered, an identical second `Notify` delivered **0** notifications.
  - The new path uses claim-after-accept (§6.3). The relay is left as is.

## 13. Android-facing details (v3.1; coordinator/Android review of the v3 summary)

### 13.1 Provider identity for the phone

- **Provider id is returned to the phone.** `device.direct_wake.register` and `device.direct_wake.targets` return `provider_id`, resolved by Harness from the phone-verified `server_id` against **live** wake connections (§6.1).
  - `targets` returns `[{server_id, provider_id, available, reason?}]`. `reason` is `provider_offline` when the owned provider is known but not live with wake.
  - `register` returns `{route_id, provider_id, activation_code, expires_at}`.
- **This is the only provider ↔ backend mapping the phone uses for correlation.** There is no manual alias.

### 13.2 Activation flow: order, persistence and retry

1. **Register with Harness first.** The phone calls `device.direct_wake.register(server_id)` over its Harness session. The result is the one-time `activation_code` (TTL 10 min).
2. **Persist before network.** The phone **write-ahead persists** `{backend_id, route_id, provider_id, activation_code, expires_at}`, sealed like the enrollment flow, before step 3.
3. **Activate on the direct server.** The phone calls `POST /direct/v1/wake/subscribe {route_id, activation_code}` with **its direct bearer token** for that backend.
4. **Poll until settled.** The phone polls `POST /direct/v1/wake/subscriptions` until the route's state is `active` or `failed`. Activation is asynchronous through the host outbox, normally within seconds.
5. **Drop the code.** On `active`, the phone deletes the persisted code and keeps `{route_id, provider_id}`.

**Retry and crash rules.**
- **Death before step 2** (code never persisted). The route stays pending and inert. The phone simply registers again, which revokes the pending route (`replaced`) and mints a new code.
- **Death after step 2.** The phone repeats step 3 with the same code. Same grant + route + code is idempotent and returns the current state.
- **Code expired, or `failed:activation_expired`.** The phone re-registers (new route, new code).
- **`failed:route_gone`.** The phone re-registers once. If that also fails, it shows the failure; there is no loop.
- **Code handling.** The code appears only in the register response, the sealed phone store, and the direct subscribe request body. It **never appears in FCM, logs, URLs or browser storage**. Browser clients cannot register at all (§6.1).
- **Fresh direct grant (reauthorization)** on the same backend. Once the new grant is installed, the phone re-runs steps 1–5.
  - Registering replaces the old route (`replaced`).
  - The old grant's subscription disappears when the old grant is retired or revoked.
  - Until the new route is active, no wake is delivered for this backend. The phone shows "reconnecting".

### 13.3 Live-provider requirement

- **Setup needs the provider online.** Register and activation answer `wake_unavailable` (reason `provider_offline`) unless the owned provider is live with `direct-wake-v1`. The phone shows "Machine's Harness connection is offline", and setup stays unavailable rather than queued.
- **An activated route survives provider disconnects** (§6.5).

### 13.4 Phone gate for an incoming wake

The phone draws nothing directly from a wake. It only decides whether to start a direct fetch, and it drops the wake with no network call unless all of these hold:

- `kind = direct_wake` and `v = 1`;
- `route_id` maps to a locally enabled, non-removed, unfenced backend whose persisted route is active;
- the envelope `user_id`, `harness_client_id`, `origin` and `authorization_generation` exactly match the **values saved at route activation** for that backend (§14). It does **not** require an unexpired cloud session.
- the backend is not locally fenced by sign-out, server change or removal;
- the backend's direct credential is usable. The direct fetch uses only the direct credential.

The existing cloud `PushGate` for relay content and actions is unchanged.

### 13.5 Fetch: cursor, gap, snapshot and ack rule

- **Incremental fetch.** `POST /direct/v1/wake/events {route_id, after_seq, limit}`.
  - It returns records with `seq > after_seq`, ascending, including state changes. A record answered elsewhere comes back with `state:"resolved"` or `"stale"`, so the phone clears that card.
  - `next_seq` is the highest returned `seq`.
  - The phone persists `next_seq` per backend only after its post-or-drop decision for every returned item.
- **Gap.** `gap:{through_seq, reason:"cap"|"expired"|"filtered"}` appears when records in `(after_seq, through_seq]` no longer exist. The phone must then do a snapshot read.
- **Retention never silently drops an open permission.**
  - The 128-record cap evicts, oldest first, only **non-open** records (turn ends, resolved, stale).
  - Open permissions are bounded separately at 256 per subscription. They are naturally few, because the runtime blocks on them.
  - Past that bound, the oldest open records are dropped, the subscription records `open_overflow`, and the gap names it.
- **Snapshot (the pending reconciliation).** `POST /direct/v1/wake/events {route_id, snapshot:true, after_seq?, limit≤64}` pages, ascending by seq, through **every un-acked record of this subscription that is still pending**: permissions `open`, and un-acked turn ends.
  - Each page carries `complete:bool`. `complete:true` appears on the final page **only** when no pending record was ever dropped for this subscription since the phone's last complete snapshot.
  - Otherwise the final page carries `complete:false, reason:"open_overflow"|"expired"`. The phone must then reconcile truthfully: for each in-scope session it shows, it reads open permissions from `history`, using bounded backward pages to the incarnation start or a 512-event bound, and reports "may be incomplete" if that bound is hit. It never claims completeness.
  - The response includes `next_seq` = the current highest seq, so incremental reads resume without a gap.
- **Bounded display lookup.** For a fetched item, the phone reads exactly one event with `GET /direct/v1/sessions/{sid}/history?before_seq={ordinal+1}&limit=1`. `before_seq` is the exclusive upper ordinal (`server.go` `history`), so this returns the event at `ordinal`. It needs `read` and scope. There is no unbounded history scan.
- **Records answered elsewhere.** A permission resolved on the machine (by any client, the CLI, or the relay) becomes `resolved`. It is returned once by incremental reads and is absent from snapshots.
- **Ack rule.** The phone acks a `wake_id` **after** it has posted or deliberately dropped the card. That covers drawn, collapsed into another card, out of scope, or resolved. An un-acked record is re-offered by snapshot until it is acked or reaches its 24 h or 128-record bound.

### 13.6 Session incarnation exposure

- **Every surface carries the incarnation (additive).**
  - The direct `GET /direct/v1/sessions` and watch rows gain `incarnation`, the daemon store incarnation.
  - The harnessproto `Session` inventory row gains `incarnation`, published under the `sessions` feature.
  - The Harness relay push gains `session_incarnation` when its inventory has it (§6.4).
- **Watches are per session instance.** `POST /direct/v1/wake/watch` takes `{route_id, session_id, incarnation, watch}`. A mismatch with the current incarnation answers `409 incarnation_changed`, and the phone refreshes the row.
- **Older servers send no incarnation.** The field is absent, the phone does not offer per-session watches, and correlation never matches.

### 13.7 Explicit Harness logout, re-login and renewal

- **Explicit logout or server change.** `device.direct_wake.sign_out` revokes all routes server-side, and the phone fences locally (§14).
- **Re-login never re-activates automatically.** On the phone, the toggle stays on, the card reads "Paused — signed out of Harness", and **an explicit tap re-runs §13.2**.
- **Cloud session expiry (8 h) does not pause wakes.** There is no silent Google or cloud re-auth; wake authority is route-bound (§14).

## 14. Route-bound wake authority (v3.3)

This is a separate **notification registration lifetime** for content-free wakes. It is not a refresh of Google or cloud authorization.

- **Binding.** Explicit opt-in (register + activate, §13.2) needs a **live authenticated cloud principal**. The route then durably binds `user_id`, `client_id`, `origin` and `authorization_generation`. The phone saves the same four values with the backend at activation.
- **Delivery check, at send time, in one statement.**
  - The route is active and not revoked.
  - `app_users.status = active`, and its generation equals the route's.
  - `user_clients` is not revoked, kind android, same origin.
  - A device row matches (U, C).
  - **No live `user_sessions` row is required.** Only an opaque wake is delivered: no content and no action capability.
- **What terminates the binding, server side.**
  - `device.direct_wake.sign_out` (explicit sign-out or server change) revokes all of the client's routes. It needs a live session, and the phone calls it before `DELETE /api/auth/session`.
  - `device.direct_wake.unregister`.
  - `RevokeUserClient`, `RevokeAllUserAuthority`, a generation bump, or the account being disabled.
  - Provider owner change (§6.2), direct unsubscribe or grant revoke (route sync).
- **What does not terminate it.** Cloud session expiry, and generic `RevokeUserSession` (a session delete is not intent).
- **Local fence (backstop).** Before the sign-out network calls, the phone persists, sealed, a per-backend `paused_signed_out` fence.
  - From then on, the §13.4 gate drops every wake for that backend.
  - If `sign_out` could not reach Harness (offline), the phone keeps a pending sign-out intent and replays it on the next authenticated session, before anything else.
  - Until that replay, Harness may still send opaque wakes to the paused phone. They are dropped locally and carry no content.
- **Account change** (a different user, origin or generation saved vs. current sign-in) means the saved binding no longer matches any new authority. The backend shows "Paused"; re-activation needs an explicit tap.
- **Security tradeoff vs. device registration.**
  - `devices` rows already outlive cloud sessions, and relay content pushes gate on a live session (`ActivePushAuthority`) because they carry content.
  - A direct wake carries none. Its worst case after session expiry is metadata timing (route_id, wake_id, timestamp) reaching a device of a still-unrevoked client of an active user.
  - That is the same population that already holds a registered FCM token.
  - `user_clients` rows have no expiry and are revoked only explicitly (`RevokeUserClient`, `RevokeAllUserAuthority`; `store/user_identity.go`), so keeping a client active after session expiry is the existing model, not a new one.
  - Reviewer: please flag a blocker if you find a source path where an expired-session client should be considered inactive.

## 15. Evidence status (review P1–P4)

| Item | Status |
|---|---|
| **P1:** a waiting session is never reported idle | **not established**, so the dependency is removed (§4.3): idleness never stales a permission |
| **P2:** ordinal survival / truncation | **not claimed**. `Head` < mark is defensive; incarnation change is primary; both are tested with a scripted backend |
| **P3:** live connection index | new code. A required test: a disconnected provider with stored `capabilities.directWake` is **not** resolvable by `register`/`targets`/activation (`hub.go` today keeps only the enroll advert per connection, L283–288) |
| **P4:** re-sign-in to the same account | does **not** revoke routes (§6.1 `sign_out` is explicit only). A required test: interactive re-sign-in, same user and client, keeps the routes deliverable |

## 12. Review findings → v3

| Source | Finding | v3 |
|---|---|---|
| C1 / R1 | Coexistence | Harness suppresses nothing. Phone-side per-event collapse; direct wins only when its fetch covers the tuple; never a relay action for a direct-owned card; timing bounds stated (§6.4) |
| C2 / R2 | Atomicity | option (a): everything in `grants.json`; nonce, not counter; old-binary rewrite then re-enable test (§4.1, §4.4) |
| C3 / R3 | Pre-send loss on host and Harness | leased outbox + settle after the answer (§4.5); Harness claim-after-accept with release on transient error (§6.3) |
| C4 / R4 | Detector | monotonic durable mark; dedupe on the mark; baseline; round-robin + `degraded` (§4.3) |
| C5 / R5 | Foreign mutation; stale serverId; send recheck | no mutation + indistinguishable answer; connection-level `unavailable`; one-statement send-boundary check (§6.2, §6.3) |
| C6 / R6 | Possession binding, squat, reauthorization, logout | authenticated activation code + grant_ref; re-register on a new grant; sign-out revokes all of the client's routes; session expiry ≠ sign-out (§3, §6.5) |
| C7 / R7 | Paging, staleness, CORS | seq cursor; resolved/stale; incarnation in identity; POST/GET only, route_id never in URLs; preflight tests (§4.2–4.4) |
| R-A | Notify is owner-wide | separate `DirectWakeDelivery` to one client (§6.3) |
| R-B | Watch incarnation | watches bound to the incarnation; deleted on change (§4.3) |
| R-C | Stored advert is stale | live hub index only (§3, §6.1) |
| R-D | Silent truncation | `gap` marker (§4.4) |
| R-E | server_id in FCM | removed (§5.2) |
| Root | Prove wake-only discovery and registration | `targets` + register by server_id from the live index; e2e with the other features off (§6.1, §8) |
| b9731f | v2 bound routes to the registering session, but sessions renew every ≤ 8 h | bind to client + generation; explicit sign-out revokes; expiry only pauses (§6.5) |
| Root (v3.2) | Shared mark skips backlog; subscribe squatting; ledger claimed before send; lost activation ack; approve and turn_end fuzziness; Android fields | per-subscription marks advanced atomically (§4.3); no global reservation (§4.2); pending-lease/accepted ledger and at-least-once (§6.3); idempotent activation + register `request_id` replay/`superseded` (§6.1, §6.2); view-only direct card and exact turn_end with relay `ordinal` + `session_incarnation` (§6.4); provider_id in register/list/targets, watch incarnation response, no title/body (§5.2, §13) |
| Root + reviewer (v3.3) | `RevokeUserSession` is not sign-out intent; session TTL must not cap wakes | route-bound wake authority without a live-session requirement; explicit `device.direct_wake.sign_out` + local fence + pending-intent replay; snapshot paging with truthful `complete`; open permissions never evicted silently; one-event display lookup (§13.5, §14) |
| Reviewer (v3.4) | R1 wake before activation; R2 sync revokes fresh route; R3 shared-file coupling; R4 sign-out intent; R5 session gap | activation-first leasing + `unavailable` for pending (§4.5, §6.2); epoch-complete sync including activating, with a 60 s grace (§4.6); authority in `grants.json` + separate `wake.json`, failure-order tests (§4.4); `sign_out` with a reason, local drop first, re-sign-in keeps routes (§6.1); R5 superseded by route-bound authority (§14) |
