# Direct wake-ups: contract v4.1 (consolidated)

**Status: converged.**
- **Implementation is authorized.** Backend implementation, including wire and API, is authorized on this branch and the harness branch.
- **Schemas** in §9–§10 are the integration baseline for Android.
- **History.** Earlier texts (v1–v3.4) are superseded. The resolution log is in §17.

Owner: b9731f (backend: agent-multiplexer + harness). Android: ba9a3a. Reviewer: 87711e.

## 1. Summary

An Android client that talks to a machine **directly** (`amux direct`, its own grant) may opt in, per direct backend, to **content-free cloud wake-ups**.

- **Detection.** The host's direct server watches the events the cloud relay already notifies on: `permission_request`, and `turn_end` for session instances watched per subscription.
- **Host wake store.** The host writes a wake record and an outbox item to its own wake store. Wake authority lives in `grants.json` (§6.2).
- **Forwarding.** `amux provide` forwards an opaque `(route_id, wake_id)` to Harness over its authenticated connection.
- **Delivery.** Harness checks route-bound authority from server state only, then sends a data-only FCM message to the devices of **one** Android client.
- **Fetch and actions.** The phone fetches details directly with its own direct credential, and answers with the existing direct `permission` verb.

**Invariants.**
- Harness and FCM never see session content, titles, prompts, permission text, request ids, runtime generations, or direct tokens, verifiers or bundles.
- Relay notifications are unchanged. Coexistence is decided on the phone, per exact event (§12).
- A wake-path failure never impairs ordinary direct operation (§6.2).

## 2. Source map (audited; agent-multiplexer and harness origin/master, 2026-11-15)

| Concern | Location |
|---|---|
| Relay push: kinds, owner-wide fan-out, authority envelope, dedupe ledger claimed before send | harness `backend/push/push.go` |
| Relay daemon ingest → push (needs provider `sessions` + `runtime-events`) | `backend/engine/provider/runtime_events.go` |
| Strict owner resolution | `backend/push/owner.go` |
| Device registry; relay watch `(owner, scope, session_id)` | `backend/store/devices.go`, `migrations/0012_devices.sql` |
| Beta identity: users, clients (no expiry, revoked only explicitly), sessions | `backend/store/user_identity.go`, `beta_write_authority.go` |
| Session TTL 8 h, a new row per sign-in. `DELETE /api/auth/session` is the only `RevokeUserSession` caller. Client credential `X-Harness-Client` | `backend/api/client_sessions.go` (L96–100, L326–341, L462–474, L918); `main.go` L448–453 |
| Provider capabilities persist after disconnect; the live advert is per hub connection | `backend/store/providers.go`; `engine/provider/hub.go` L283–288 |
| The backend catalog requires the enroll advert | `backend/directenroll/advert.go` L47; `api/backends.go` L48 |
| Direct server routes; CORS GET/POST only | amux `internal/direct/server.go` |
| Authority: grants, leases, revoke, `commitLocked`, degraded mode | `internal/direct/authority.go` |
| `grants.json` (single file, lenient decode, `writeAtomic`) | `internal/direct/store.go` |
| Scope filter | `internal/direct/sessions.go` (`inScope`, `filtered`) |
| `Snapshot`; `History` (exclusive `before_seq`); `Stream(after)`; `SessionIdentities` (incarnation) | `internal/direct/sessions.go`; `internal/store/identity.go` |
| Admin socket, provider bridge, cloud pin resolver | `internal/direct/admin.go`, `relay.go`, `cloud.go`; `cmd/amux/direct_cloud_pins.go` |
| Provider advert loop, `registered.owner`, runtime-events pump | `internal/provider/directenroll.go`, `provider.go`, `runtimeevents.go` |
| Verifier/challenge precedent | `docs/direct-cloud-enrollment.md` (PKCE S256) |

## 3. Events (no new alert types)

| Relay today | Direct wake | Condition |
|---|---|---|
| daemon `permission_request` | `permission_request` | every live event carrying the exact `item_id` + `request_id` + `runtime_generation` |
| watched `turn_end` | `turn_end` | only for a session **instance** (id + incarnation) watched by that subscription on the host. The relay `device_watches` is not used. |

Not wake-worthy:
- events at or below the subscription's own mark;
- synthetic reconcile turn ends;
- inventory state changes.

## 4. Identities: metadata vs. secrets

| Value | Issued by | Kind | Where it may appear |
|---|---|---|---|
| `user_id`, `client_id` (android), `origin`, `authorization_generation` | Harness | metadata | Harness, FCM envelope, phone |
| `provider_id`, `server_id` | Harness / direct server | metadata | Harness, phone. **Not in FCM.** |
| direct grant `client_id` | direct server | metadata | host and phone only |
| `grant_ref` = b64url(HMAC-SHA256(host policy key, grant client_id))[:22] | host | metadata, opaque | host, Harness |
| `route_id` = `dwr_`+b64url(32) | Harness | metadata; **possession grants nothing** | Harness, FCM, phone, direct request **bodies** (never URLs) |
| `wake_id` = `dwk_`+b64url(16) | host | metadata, unordered | host, Harness, FCM, phone |
| `request_id` = b64url(16..32 random bytes) | phone | metadata | phone, Harness |
| `activation_secret` = the unpadded base64url **string** of 32 CSPRNG bytes (43 characters) | **phone**, fresh per setup attempt | **secret** | sealed phone store, direct subscribe body, provider → Harness activation frame. **Never** in Harness storage, register, FCM, logs, URLs or browser storage. |
| `activation_challenge` = unpadded b64url(SHA-256(the ASCII bytes of the `activation_secret` **string**)) — exactly PKCE S256 | phone | metadata (a commitment) | register request, Harness storage |
| direct access/refresh tokens, verifier, bundles | direct server | **secret** | never on the wake path |

**Logging.**
- route_id and wake_id are logged only as an 8-character SHA-256 prefix.
- Secrets, challenges, grant client ids and session ids are never logged on the wake path.

**Shared fixture.** `harnessproto/testdata/direct-wake-v1-challenge.json` uses the RFC 7636 Appendix B vector: secret `dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk`, challenge `E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM`. `harnessproto.DirectWakeChallenge` is the reference implementation.

## 5. Binding: route-bound wake authority

**Route.** R = (U, C, P, S): the user, the Android client, the provider and the direct server.

**Registration and activation** need a **live authenticated cloud principal**. After activation, R durably binds U, C, `origin` and `authorization_generation` g. This is a notification registration lifetime and never refreshes Google or cloud auth. The **route lifetime is 90 days**, renewed only by authenticated activity of the same client while R is active and unexpired (§10.2). An expired, signed-out or revoked route is never revived.

**Proof chain.** Every link is checked from server state.

1. **U owns P.** The live provider row's owner is U. Sharing and admin are ignored.
2. **P is live with the feature.** A **live hub connection** of P negotiated `direct-wake-v1` (feature echoed), with `directWake.serverId = S`. Stored capabilities are never used.
3. **The host pins U and P.** The host's wake policy pins them, defaulted from its live registration.
4. **C committed to the activation.** C registered R with `activation_challenge` = SHA-256 of a phone-generated secret, under `lockActiveUserAuthority(U, g)`.
5. **Activation binds R to one grant.** The phone presents `activation_secret` to S **with grant G's bearer token**. The host relays `{route_id, activation_secret, grant_ref, requester?}` through P. Harness checks that SHA-256(secret) equals the stored challenge, then checks TTL and P/S, then marks R `active` bound to `grant_ref`.
6. **Brokered grants.** If G was cloud-brokered, `G.CloudRequester` must equal U.

**Delivery-time authority.** One SQL statement runs immediately before each FCM send. It requires all of the following:
- R is active, not revoked, and not expired;
- U is active, and `authorization_generation = g`;
- C is not revoked, kind android, same origin;
- a device row exists for (U, C).

**No live `user_sessions` row is required.** Only an opaque wake is delivered. The cloud content/action gate (`ActivePushAuthority`, Android `PushGate`) is unchanged.

**What terminates the binding:**
- client-credential `wake-sign-out` (§10.2);
- `device.direct_wake.unregister`;
- client revoke, revoke-all, a generation bump, or the account being disabled;
- a provider owner change;
- a direct unsubscribe or grant revoke, through route sync;
- 90 days without renewal.

**What does not terminate it:**
- cloud session expiry;
- `RevokeUserSession` / `DELETE /api/auth/session` (not sign-out intent; Android also calls it on interactive re-sign-in);
- interactive re-sign-in to the same account.

**Security tradeoff.**
- `devices` rows already outlive sessions.
- `user_clients` have no expiry and are revoked only explicitly.
- The worst case after session expiry is wake timing metadata reaching a device of a still-unrevoked client of an active user. The 90-day lifetime bounds it.

## 6. Host (agent-multiplexer)

### 6.1 Opt-in (off by default)

- **Host policy.** Admin ops `wake_enable`, `wake_disable`, `wake_status` (SubjectHost only). CLI: `amux direct wake enable [--owner --provider]`, `disable`, `status [--json]`. Pins default from the live provider registration through the cloud resolver.
- **Provider.** `amux provide --direct-wake` (`AMUX_PROVIDER_DIRECT_WAKE=1` / `direct-wake = true`). The provider advertises `direct-wake-v1` + `capabilities.directWake.serverId` only while the policy is enabled with matching pins. It re-checks every 30 s and re-registers after two consecutive changes.
- **Not required:** `sessions`, `runtime-events`, `direct-enroll-v1`.

### 6.2 Durability: authority in `grants.json`, wake data in `wake.json`

1. **Authority in `grants.json`, under the existing commit semantics.**
   - `WakePolicy {enabled, nonce (16 random bytes, new on every change), owner, provider, key (32 bytes, new per nonce), changed_at}`.
   - `grantRecord.WakeSubs[route_id] = {created_at, nonce, state: activating|active|failed, failure?, challenge_digest}`. `challenge_digest` is the SHA-256 of the presented secret, kept so a re-subscribe can be recognized.
   - These change only on low-frequency authority actions: enable, disable, re-pin, subscribe, unsubscribe, activation settle, grant revoke or expiry, `reset-identity`.
2. **Wake data in `wake.json`.**
   - Mode 0600, a sibling file, written with `writeAtomic` under its own lock.
   - It holds per-subscription marks, incarnations, watches, records, tombstones and the outbox, each keyed by `(grant client_id, route_id, nonce)`.
   - Writes are batched: at most one fsync per 250 ms, plus an immediate one after a settle.
3. **Fence first, no two-file atomicity.** Revoke, disable, re-pin and reset commit the authority fence in `grants.json` **first**. `wake.json` cleanup follows, best-effort.
4. **Every wake operation re-checks authority.** Detection, record, lease, settle, fetch, ack, watch and sync each re-check that:
   - the grant is live;
   - the policy is enabled with the same nonce;
   - the `WakeSubs` entry exists in the required state.

   An entry that fails is **inert** and is deleted lazily.
5. **Failure isolation.**
   - A rolled-back `wake.json` write reverts in memory.
   - An ambiguous one sets **wake-degraded**: wake operations are refused, and `wake status` and capabilities report it, until a restart reconciles.
   - It **never** sets the authority's `degraded`, and it never blocks token issuance or actions.
6. **Startup reconciliation.** Drop wake entries whose grant is absent, revoked or expired, whose nonce is not current, or whose subscription is gone. An older restored `wake.json` can only cause a duplicate wake, never a loss: marks only go up, and records are re-derived from the event stream.
7. **Locks.** Authority checks are a brief read under the authority lock, taken before the wake lock. Wake writes never run under the authority lock.

**Old-binary rewrite.** It drops `WakePolicy`/`WakeSubs`, so wake is off and `wake.json` is inert. Re-enable mints a new nonce.

### 6.3 Direct API (GET/POST only; bearer access token; `read`; no new permission)

- **Request rules.** JSON bodies. Unknown fields or query parameters → `400 bad_request`. route_id is never in a path or query.
- **Grant kinds.** Every kind (manual, PKCE-brokered, device ticket) opts in without re-enrollment.

| Route | Body | Result |
|---|---|---|
| `POST /direct/v1/wake/subscribe` | `{route_id, activation_secret}` | Creates or updates this grant's `WakeSubs` entry (`activating`) and queues an `activate` outbox item. `200 {route_id, state, failure?, created_at}`. Same grant + route + secret is idempotent. Same grant + route with a **new** secret replaces the pending activation and re-queues it (this cannot downgrade an `active` entry: `409 already_active`). **No global reservation:** several grants may hold `activating` entries for one route; only the proof Harness accepts becomes `active`, and the rest settle `failed:route_gone`. `409 wake_disabled`. `409 capacity_exhausted` (more than 4 per grant). |
| `POST /direct/v1/wake/unsubscribe` | `{route_id}` | Removes the entry (authority commit) and queues `deactivate`. `200 {removed}`. Idempotent. |
| `POST /direct/v1/wake/subscriptions` | `{}` | `{"subscriptions":[{route_id, state, failure?, created_at, watches:[{session_id, incarnation}]}]}` |
| `POST /direct/v1/wake/watch` | `{route_id, session_id, incarnation, watch}` | Adding requires the session in scope (`404 not_found`) and a current `incarnation` (otherwise `409 incarnation_changed`, with `incarnation` = the current one). Removing needs no access. At most 64. `200 {route_id, session_id, incarnation (current), watched}`. |
| `POST /direct/v1/wake/events` | §8 | §8 |
| `POST /direct/v1/wake/ack` | `{route_id, items:[{wake_id, seq}] ≤64}` | `200 {acked}`: the count of items whose `seq` matched the record's **current** revision. Stale seqs are ignored (§8.4). |

**Other additive changes.**
- `GET /direct/v1/capabilities` gains `wake:{v:1, enabled, degraded}`.
- Direct session rows (list and watch) gain `incarnation`.
- An old server answers 404 with no JSON code.

### 6.4 Leased, retriable outbox

**Item states and deadlines.**

| Item | States | Deadline |
|---|---|---|
| `activate` | `pending` / `done` / `failed` | 10 min |
| `deactivate` | `pending` / `done` | 24 h |
| `wake` | `pending` / `delivered` / `gone` / `expired` | 900 s after creation |

**Activation first.** A subscription's `wake` items become leasable only once `WakeSubs.state = active`, which happens after its `activate` item settled `accepted`.

**Lease.** `wake_lease {max≤16}` is a long-poll under 15 s. It returns `lease_id` + items. Leases live in memory for 30 s.

**Settle.** `wake_settle {lease_id, results}` runs after the Harness answer:

| Harness answer | Result |
|---|---|
| `accepted` or `no_recipient` | `delivered` (or `done`) |
| `route_gone` | `gone`; subscription `failed:route_gone` |
| `unavailable`, `rate_limited`, `busy`, frame timeout, disconnect | `pending`, with a 2–60 s backoff |
| missing settle | lease expiry returns the item to `pending` |

**Delivery guarantee.** At-least-once, with the same `wake_id` on every send. Harness suppresses re-sends it recorded as accepted. **Exactly-once is not claimed.**

**After a revoke.** An item whose subscription fails the authority check is never leased, and a settle for it is ignored. At most one wake already submitted to Harness can still be delivered; its fetch then answers `401` or comes back empty.

### 6.5 Route sync

- **Cadence.** On every registration and every 30 s, the provider sends `direct-wake-routes {serverId, epoch, routes:[{routeId, grantRef, state}], part, final}` (≤ 512 per part).
- **Contents.** It lists every `activating` and `active` subscription.
- **Epochs.** Harness applies only a **complete epoch**: every part, same epoch, same connection.
- **Scope.** It applies only to routes with `provider_id = conn.providerID` and `server_id = serverId`.
- **Misses.** An active route absent from two consecutive complete epochs is revoked (`deactivated`). Misses count only after the route has been active for one full sync interval (30 s).
- **Ignored.** Unknown or foreign ids, and `grantRef` mismatches.

## 7. Detection

### 7.1 Session set

The union of in-scope sessions over subscriptions: valid id, not console, not effectively archived.

### 7.2 Per-subscription marks

- **Marks.** Each (subscription, session) pair has a durable `mark[sub][sid]` that only goes up. There is **no shared mark**.
- **A new subscriber.** When a session enters a subscription's set, only that subscription's mark is set, to the current `History` `Head`. Other subscriptions keep their backlog.
- **Reading.** The detector reads each session from the minimum mark across subscriptions.
  - Up to 64 live sessions are tailed with `Stream`.
  - The rest are polled round-robin with `History` back to the minimum mark: at most 8 pages per turn, continued next turn, never skipped.
- **Check interval.** Each session is checked at least every `max(2 s, ⌈N/16⌉×2 s)`. Lag above 60 s sets `degraded`.
- **Evaluation.** For each event at ordinal `o`, each subscription with `mark < o` evaluates it. Its records and its new mark go into the same `wake.json` batch. A crash before durability re-evaluates the event, and identity tombstones dedupe it.
- **Incarnation (primary).** On a new incarnation, or when the session is gone:
  - all marks for that session reset to the new `Head`;
  - the old incarnation's watches are deleted;
  - its open permissions become `stale`.

  `Head` below the minimum mark triggers the same reset as a defensive measure.

### 7.3 Identities and permission state

- **Permission identity:** `sha256(sid, incarnation, "permission_request", item_id, request_id, runtime_generation)`, length-prefixed.
- **Turn-end identity:** `sha256(sid, incarnation, "turn_end", ordinal)`.
- **Permission state** is `open`, `resolved` or `stale`, and is **separate from delivery ack** (§8.4).
  - `resolved`: a matching `permission_resolved` was seen.
  - `stale`: a later event carries a different non-empty runtime generation, or the session was reset.
  - Idleness never stales a permission.
- **New sequence number.** Every state change gives the record a **new `seq`**.
- **Resolution wake.** When a `delivered` record becomes `resolved` or `stale`, the host queues one new `wake` (new wake_id) so the card clears promptly. This is best-effort.

## 8. Fetch, snapshot and ack

### 8.1 Sequence

`seq` is per subscription and monotonic. It is assigned at creation and on **every** state change, and it is the only order and the only cursor. wake_ids are unordered.

### 8.2 Incremental fetch

`POST /direct/v1/wake/events {route_id, after_seq, limit≤64}`:

```json
{"route_id":"dwr_…","items":[
  {"seq":41,"wake_id":"dwk_…","kind":"permission_request","state":"open","acked":false,
   "session_id":"…","incarnation":"…","item_id":"…","request_id":"…","runtime_generation":"…","ordinal":123,"created_at":1731700000000},
  {"seq":44,"wake_id":"dwk_…","kind":"permission_request","state":"resolved","acked":true,"session_id":"…","incarnation":"…","item_id":"…","request_id":"…","runtime_generation":"…","ordinal":118,"created_at":1731699990000}],
 "next_seq":44,"has_more":false,"gap":null}
```

- **Contents.** Records whose current seq is greater than `after_seq`, ascending.
- **Filtered at fetch time.** A record is returned only if all of these hold now:
  - the grant is live with `read`;
  - the nonce is current;
  - the session is in scope and not console;
  - the incarnation is current.

  A record that fails is tombstoned.
- **Unknown or foreign route.** It returns `200 {"items":[],"next_seq":after_seq,"has_more":false,"gap":null}`.
- **Gap.** `gap:{through_seq, reason:"cap"|"expired"|"open_overflow"}` means the phone must take a snapshot.
- **Cursor persistence.** The phone persists `next_seq` only after its post-or-drop decision on every item.

### 8.3 Snapshot (bounded, high-water-captured)

1. **First page.** `{route_id, snapshot:true, limit≤64}` captures `snapshot_hw` = the current highest seq and returns it.
2. **Later pages.** `{route_id, snapshot:true, snapshot_hw, after_seq, limit}` return only records with seq ≤ `snapshot_hw`.
3. **What is included.** **Every `open` permission**, acked or not, plus un-acked turn ends.
4. **Final page.** `has_more:false`, `next_seq = snapshot_hw`. Records appended or resolved during paging have seq > `snapshot_hw`, so the next incremental fetch returns them.
5. **Completeness.**
   - `complete:true` only if no open permission or un-acked turn end was dropped since the phone's last complete snapshot.
   - Otherwise `complete:false, reason`. The phone then reconciles open permissions from `history`, reading bounded backward pages (≤ 512 events per session), and shows "may be incomplete" if it hits the bound.

### 8.4 Ack vs. pending state

- **Acks are versioned.** An ack names `{wake_id, seq}`. It applies only if `seq` equals the record's current revision. An ack carrying an older seq is ignored, so it can never delete a later `resolved`/`stale` revision the phone has not seen.
- **Every state change resets the ack.** It assigns a new seq **and** sets `acked:false`.
- **Ack means "seen":** posted, or deliberately dropped. It sets `acked:true` for that revision and **never** changes pending state.
- **An acked `open` permission** stays pending (in snapshots), and still receives `resolved`/`stale` revisions.
- **Removal.**
  - An acked turn end is removed and tombstoned.
  - A `resolved`/`stale` permission is removed only once **its own** revision is acked, or when retention ends.
- **Retention never evicts an open record.**
  - The 128-record cap evicts only non-open records, oldest first.
  - Open permissions have a separate cap of 256. Past it, the oldest are dropped with `open_overflow`.
  - Records last 24 h.

### 8.5 Display and actions

- **Display lookup.** One event: `GET /direct/v1/sessions/{sid}/history?before_seq={ordinal+1}&limit=1`.
- **Answering.** The existing `POST /direct/v1/sessions/{sid}/actions {"verb":"permission", request_id, runtime_generation, decision, idem_key}`, which requires `approve`.

## 9. Wire

### 9.1 harnessproto `direct-wake-v1` (additive; golden fixtures)

| Frame | Direction | Body |
|---|---|---|
| `registered.features` | orch → prov | the features accepted. Wake frames are sent only after the echo. |
| `direct-wake-activate` | prov → orch | `{type, reqId, directWake:{serverId, routeId, activationSecret, grantRef, requester?}}` |
| `direct-wake` | prov → orch | `{type, reqId, directWake:{serverId, wakes:[{routeId, wakeId, grantRef}] ≤16}}` |
| `direct-wake-routes` | prov → orch | `{type, reqId, directWake:{serverId, epoch, routes:[{routeId, grantRef, state}] ≤512, part, final}}` |
| `direct-wake-result` | orch → prov | one per reqId, within 15 s: `{type, reqId, directWakeResult:{results:[{routeId, wakeId?, status}], error?}}` |
| `runtime-events` (existing) | prov → orch | additive `incarnation`: the provider stamps the session's store incarnation on each batch |

- **Statuses:** `accepted`, `no_recipient`, `route_gone`, `rate_limited`, `unavailable`, `busy`.
- **Connection-level mismatch** answers the whole frame with `error:"unavailable"`, never `route_gone`.
- **Flow control.** At most 4 frames in flight.
- **Compatibility.**
  - An old Harness never echoes the feature, so nothing is sent and activation expires.
  - An old provider makes registration answer `wake_unavailable`.
  - An old direct server answers 404 with no JSON code.

### 9.2 FCM data payload (data-only, `priority: high`, `ttl: 900s`)

```json
{"kind":"direct_wake","v":"1","route_id":"dwr_…","wake_id":"dwk_…","ts":"RFC3339",
 "harness_client_id":"…","user_id":"…","origin":"…","authorization_generation":"…"}
```

There is no title, body, kind, session, request, generation, item, server id, Harness session or expiry.

## 10. Harness

### 10.1 Schema (migration `0022_direct_wake_routes.sql`; in beta export/delete/restore)

```sql
CREATE TABLE direct_wake_routes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES app_users(id),
  client_id TEXT NOT NULL REFERENCES user_clients(id),
  origin TEXT NOT NULL,
  provider_id TEXT NOT NULL,
  server_id TEXT NOT NULL,
  authorization_generation BIGINT NOT NULL,
  request_id TEXT NOT NULL,
  activation_challenge TEXT NOT NULL,          -- b64url(SHA-256(secret)); kept for idempotent re-activation
  activation_expires_at TIMESTAMPTZ NOT NULL,  -- created_at + 10 min
  grant_ref TEXT,
  activated_at TIMESTAMPTZ,
  renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,             -- renewed_at + 90 days
  sync_misses INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  revoke_reason TEXT);
CREATE UNIQUE INDEX direct_wake_routes_active ON direct_wake_routes(user_id,client_id,provider_id,server_id) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX direct_wake_routes_request ON direct_wake_routes(client_id, request_id);
CREATE TABLE direct_wake_deliveries (
  device_id TEXT NOT NULL,
  wake_key TEXT NOT NULL,
  state TEXT NOT NULL,                         -- pending | accepted
  lease_until TIMESTAMPTZ,
  accepted_at TIMESTAMPTZ,
  PRIMARY KEY(device_id, wake_key));
```

A live-connection index (in memory, per broker) maps each provider to its negotiated `serverId`. It is maintained on register and disconnect.

### 10.2 Verbs and endpoint

**`/api/ws` verbs** need a beta Android principal with a **live** session. Legacy, email-only and browser identities are refused, with no fallback.

| Verb | Fields | Result |
|---|---|---|
| `device.direct_wake.targets` | — | `{targets:[{server_id, provider_id, available, reason?}]}`. `reason` is `provider_offline` when the provider is not live with the feature. |
| `device.direct_wake.register` | `server_id`, `request_id`, `activation_challenge` | `{route_id, provider_id, state:"pending", activation_expires_at, expires_at}`. Detailed below. |
| `device.direct_wake.list` | — | `{routes:[{route_id, server_id, provider_id, state:"pending"\|"active"\|"expired"\|"revoked", revoke_reason?, activation_expires_at, expires_at, created_at}]}`. Renews active, unexpired routes. |
| `device.direct_wake.unregister` | `route_id` | `{removed}`. Owner- and client-scoped. Idempotent. |

**`register` behavior.**
- **Provider resolution.** It resolves the provider from the live index and requires exactly one owned provider. Zero answers `wake_unavailable`; more than one answers `wake_ambiguous`.
- **Requirements.** At least one device row for (U, C); the write runs under `lockActiveUserAuthority`; at most 8 active routes per client.
- **New `request_id`.** The existing route for (U, C, P, S) is revoked (`replaced`), and a new route is minted with the challenge.
- **Same `request_id` and same challenge.** Returns the same route and its current state, whether still pending or active, so a lost or reordered response is safe.
- **Same `request_id`, different challenge.** `409 challenge_mismatch`, with no mutation.
- **Same `request_id` but the route was replaced or revoked.** `409 superseded`.
- **Same `request_id` but activation expired unactivated.** `409 activation_expired`.

**Renewal.**
- Any authenticated `register`/`list`/`targets` call, or `device.register`, from client C with a live session sets `renewed_at = now()` and `expires_at = now() + 90 days`.
- This applies only to C's routes that are active, unrevoked and unexpired.

**Explicit sign-out.**
- Endpoint: `POST /api/auth/client/wake-sign-out`. It is authenticated by the **long-lived client credential** (`X-Harness-Client`) alone; no session or Google token is needed.
- Body `{reason:"sign_out"|"server_change"}`. An absent reason means `sign_out`.
- It revokes all of C's routes (`revoke_reason` = reason). It is revoke-only and idempotent: `204` for a known client, including when there is nothing to revoke. An unknown or revoked client gets `401`.
- On server change it is called before the client credential is cleared.
- `DELETE /api/auth/session` is unchanged and never revokes routes.

### 10.3 Provider-input validation (no cross-tenant mutation)

The provider is always `conn.providerID`. Foreign and nonexistent routes get **indistinguishable** answers.

| Condition | Answer | Mutation |
|---|---|---|
| Frame serverId ≠ the connection's current serverId, or the connection is not in the live index | frame `unavailable` | none |
| Route unknown, foreign provider, or serverId mismatch | `route_gone` | **none** |
| Wake for a pending route | `unavailable` (retryable) | none |
| `grantRef` ≠ the route's | `route_gone` | none |
| The route's own binding is invalid from server state: owner changed, user inactive, generation changed, client revoked, or route expired | `route_gone` | revoke this route; only for routes of `conn.providerID` |
| Activation: SHA-256(secret) ≠ challenge | `route_gone` | none (a wrong secret never burns the real one) |
| Activation: match, pending, within TTL | `accepted` | `active` + `grant_ref` |
| Activation: match, already active with the **same** grant_ref (lost ack) | `accepted` | none (idempotent) |
| Activation: match, but active with a different grant_ref, or TTL expired | `route_gone` | none |
| Activation: requester ≠ U | `route_gone` | revoke (`requester_mismatch`) |
| Over 30/min per route or 120/min per provider | `rate_limited` | — |
| Transient database error | `unavailable` | — |

### 10.4 Delivery: one client; accepted only after FCM

- **Separate path.** `push.DirectWakeDelivery` is separate from relay `Notify`. Recipients are the devices of (U, C) only.
- **Per device:**
  1. **Lease.** Write `direct_wake_deliveries` as `pending` with a 30 s lease.
     - Already `accepted` → skip the device.
     - A live lease → `busy`.
     - An expired lease → take it over.
  2. **Authority check.** Run the delivery-authority statement (§5).
  3. **Send.** The FCM send uses a **10 s timeout**, shorter than the lease.
     - Success → `accepted`, durably.
     - A 429, 5xx or timeout → delete the row.
     - `ErrTokenGone` → prune the token and delete the row.
  4. **Crash after send.** A crash after the send but before the commit may duplicate.
- **Overall answer.**
  - `accepted` when every device accepted.
  - `busy` or `unavailable` when any device is retryable.
  - `no_recipient` when there is no device row.
- **Relay ledger.** Unchanged.

### 10.5 Relay additive fields (coexistence)

The relay daemon pushes gain `session_incarnation` (from the ingested batch's `incarnation`, §9.1) and, for `turn_end`, `ordinal`. Each is present only when the provider supplied it. Relay semantics are otherwise unchanged.

## 11. Fences

| Event | Effect |
|---|---|
| Explicit sign-out or server change | local fence first (§13.3), then client-credential `wake-sign-out` revokes every route. A later login never revives them. |
| Cloud session expiry, interactive re-sign-in | no effect on delivery |
| 90 days without renewal | route expired; shown truthfully as `expired` |
| Client revoke, revoke-all, generation bump, user disabled | routes revoked in the same transaction, and checked again at send |
| FCM token rotation | the new device row for (U, C) is used |
| Phone disables wake | direct `unsubscribe` + `unregister`; either alone is enough |
| Direct grant revoke or expiry, reset-identity | authority fence → wake data inert → sync deactivates |
| Direct reauthorization | the phone reruns §13.2; the old route becomes `replaced` |
| Wake policy disable or re-pin | authority fence + new nonce; advert withdrawn |
| Provider owner change | advert withdrawn; the binding check revokes the route |
| Provider or Harness down | the outbox retries until 900 s; records stay fetchable |

## 12. Coexistence (phone-side, exact; Harness suppresses nothing)

- **Both paths keep running.** Relay pushes keep going to all owner devices. Direct wakes go to the opted-in client.
- **Permission correlation.** Only when **all** of these hold:
  - the relay `id` equals the `provider_id` of the active route for this backend (a verified pairing);
  - the relay `session_incarnation` equals the direct item's `incarnation`, and both are non-empty;
  - `session_id`, `item_id`, `request_id` and `runtime_generation` are equal and non-empty.
- **Turn-end correlation.** The same pairing, plus session, incarnation and `ordinal`. **No fuzzy windows.**
- **A covered item is direct-owned.** If direct fetched it and it is in scope, the content comes from direct and actions go only through the direct `permission` verb. Without `approve`, a **view-only direct card replaces the relay card**. A direct card never routes an action through the relay.
- **Relay cards stay** in every other case: out of scope, uncorrelated, fetch failed, no active route, or no incarnation.
- **No zero-duplicate promise.** A duplicate stays visible for at most one direct fetch, or indefinitely if the peers are older.

## 13. Phone contract (Android)

### 13.1 Wake gate

Drop the wake with no network call unless all of these hold:
- `kind` and `v` match;
- `route_id` maps to a locally enabled, unfenced backend with an active route;
- the envelope `user_id`, `harness_client_id`, `origin` and `authorization_generation` equal the values **saved at activation**. No live cloud session is needed.
- the direct credential is usable.

A wake is never posted through `PushGate`.

### 13.2 Activation

1. **Generate and persist.** Create `activation_secret` (32 CSPRNG bytes) and a `request_id`. Seal-persist `{backend_id, request_id, secret}` **before any network call**.
2. **Register.** Call `register(server_id, request_id, b64url(sha256(secret)))` with a live Harness session. On a lost response, retry with the same request_id and challenge.
3. **Persist the route.** Store `route_id` and `provider_id`.
4. **Subscribe.** Call direct `subscribe {route_id, activation_secret}` with the backend's bearer token. On a lost response, repeat it.
5. **Poll.** Call `subscriptions` until the state is `active` or `failed`.
6. **On `active`.** Delete the secret, and save the binding (user, client, origin, generation).

**Failures.**
- `409 activation_expired`, `failed:activation_expired` or `challenge_mismatch` → start over with a new secret and request_id.
- `failed:route_gone` → one fresh attempt, then show the failure.
- A new direct grant → rerun with a new secret and request_id.
- Provider offline → show "Machine's Harness connection is offline". Nothing is queued.

### 13.3 Sign-out and server change

1. Persist the local fence and drop route mappings.
2. Call client-credential `wake-sign-out(reason)`.
3. Call `DELETE /api/auth/session` and clear the client.

If offline, persist a pending sign-out intent and replay it with the client credential at the next opportunity.

Re-login does not re-activate: the backend shows "Paused — signed out of Harness" until an explicit tap. An account change also means "Paused". `expired` routes show "Wake-ups expired — tap to re-enable".

### 13.4 Reconciliation

- **When.** On every wake and on foreground.
- **Fetch.** Fetch incrementally from `next_seq`. On a gap, take a snapshot.
- **Cards.** Post, collapse or clear cards per §12, then ack. Resolved and stale items clear cards, acked ones included.

## 14. Threat boundaries

| Actor | Can | Cannot |
|---|---|---|
| Harness / FCM | see routing metadata and timing; Harness also stores the activation challenge (a hash) | see content, action ids or secrets; fetch; answer |
| Another user or provider | — | register or activate on a provider it doesn't own; mutate a victim's route (no mutation, indistinguishable answers) |
| Holder of a route_id | — | activate, fetch, revoke |
| A second grant on the host | hold an `activating` entry that never activates | squat a route; widen permissions; read other grants' records |
| U's own compromised provider | spam U's routes within the rate limits; deactivate its own routes | touch other providers' routes; forge a requester |
| Replay | — | act; re-buzz an accepted wake |
| Lost phone after revocation | receive at most one in-flight wake | fetch it (401) |

## 15. Acceptance tests

### agent-multiplexer

- **Wire.**
  - Golden fixtures for all frames, `registered.features` and the runtime-events `incarnation`.
  - A negative content test.
- **Failure order.**
  - A failed or ambiguous `wake.json` write → wake-degraded, while token issuance, actions and history keep working.
  - Revoke commits, then `wake.json` cleanup fails or crashes → reconciliation on restart, and sync drops the route.
  - An older `wake.json` restored → at most a duplicate, never a loss.
  - An old binary drops the policy → `wake.json` is inert.
  - An ambiguous `grants.json` commit during subscribe → the existing degradation.
- **Live checks.** After a revoke or policy change, every lease, fetch, ack and sync is refused.
- **Detector.**
  - A new subscription added during another's backlog doesn't skip that backlog.
  - Idle → permission within one poll interval is detected.
  - More than 64 sessions → `degraded`, never starvation.
  - A new incarnation deletes its watches and stales open permissions.
  - Resolution gets a new seq and queues a resolution wake.
- **Outbox.**
  - Wakes wait for activation.
  - Lease expiry, `busy`, `rate_limited`, `unavailable` and disconnect all lead to a re-send with the same wake_id.
  - `route_gone` → failed.
  - Deadlines.
  - A settle after a revoke is ignored.
- **Fetch.**
  - An acked open permission is still in the snapshot.
  - **An old ack sent after a resolution** (stale seq) is ignored, and the resolved revision stays until it is acked itself.
  - An append during snapshot paging is not skipped.
  - A resolve or stale change gets a new seq.
  - The gap and `complete` flags.
  - The fetch-time filter.
  - CORS preflight on every route.
  - route_id is refused in a URL.
- **Subscribe.**
  - Several grants can hold `activating` for one route.
  - A new secret replaces a pending activation.
  - An `active` route can't be downgraded.
- **Provider.**
  - Frames only after the feature echo.
  - Epoch sync includes activating subscriptions.
  - Settle only after the Harness answer.
  - A connection-level `unavailable` keeps the subscriptions.
  - Runtime-events batches carry `incarnation`.
- **End to end.** With `sessions`, `runtime-events` and `direct-enroll-v1` off: activate → wake → fetch → `permission` → ack.

### harness (real Postgres in CI; fake `push.Sender`)

- **Input validation.** A foreign-provider frame naming a victim's route causes no change and gets the same answer as an unknown route.
- **Activation and register.**
  - A lost-ack re-activation → `accepted`.
  - A wrong secret or grant → no mutation, and it doesn't burn the real secret.
  - A same request_id + challenge replay returns the same route, even mid-activation.
  - A different challenge → `challenge_mismatch`.
  - A new request_id → the old route becomes `superseded`.
  - Expired → `activation_expired`.
- **Sign-out and lifetime.**
  - Client-credential `wake-sign-out` nine hours after login revokes all routes, and a re-login doesn't revive them.
  - `RevokeUserSession` alone does nothing.
  - An expired session still delivers.
  - Client revoke, revoke-all, a generation bump or a disabled user stops delivery at send time.
  - A route expires at 90 days and is renewed by activity.
  - Interactive re-sign-in keeps routes.
- **Live index.** A disconnected provider with stored capabilities is not resolvable.
- **Ledger.**
  - A live lease → `busy`.
  - An expired lease → retry.
  - A 429, 5xx or timeout deletes the row.
  - Success is durably `accepted`.
  - The send timeout is shorter than the lease.
- **Payload.** The keys equal the allowlist, with no session or expiry.
- **Sync.** Epoch completeness, scope and grace.
- **Relay.** Existing tests unchanged; the additive `session_incarnation` and `ordinal` are present when supplied.

## 16. Landing order

1. **agent-multiplexer PR** (master-based; this branch).
2. **harness PR** (master-based). It pins harnessproto to the amux PR head for CI, re-pins to the merged amux master before it is marked mergeable, and merges after amux.
3. **Android** consumes the contract.

## 17. Review resolution log

| Finding | Resolution |
|---|---|
| Coordinator 1–7 | phone-side coexistence; leased outbox; marks; no cross-tenant mutation; authenticated activation; seq cursor; GET/POST only |
| Reviewer A–E | one-client delivery; incarnation-bound watches; live index; gap marker; no server_id in FCM |
| Root v3 1–6 | per-subscription marks; no global reservation; lease/accepted ledger, at-least-once; idempotent activation and register replay; view-only direct card; exact turn_end; Android fields |
| Reviewer R1–R5 | activation first; pending → `unavailable`; epoch sync with grace; authority/wake-file split (seven rules, §6.2); explicit sign-out; route-bound authority |
| Root v3.3 fetch | ack separate from pending; new seq on state change; snapshot includes acked open permissions; `snapshot_hw` |
| Root v4 wire | versioned acks `{wake_id, seq}`; a state change resets the ack; secret = base64url string, challenge = S256 over its ASCII bytes, with a pinned RFC 7636 fixture |
| Reviewer N1–N5 | client-credential sign-out; 90-day route lifetime with renewal; provider-stamped `incarnation`; send timeout < lease |
| Register replay | phone-generated secret + SHA-256 challenge (enrollment pattern); no server secret, keyring or new setup |
| P1–P4 evidence | idleness dependency removed; `Head` < mark defensive only; live-index test; re-sign-in keeps routes |
| Out of scope (evidence only) | relay `device_watches` has no provider key; relay `Notify` claims before send (reproduced) |
