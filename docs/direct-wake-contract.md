# Direct wake-ups — contract proposal **v3** (for review)

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
2. **Durable outbox on the host.** It records each wake atomically in `grants.json` and queues an opaque `(route_id, wake_id)` in a leased, retriable outbox.
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
| Beta identity. Android **session TTL 8 h**, and a new session row per session start (`api/client_sessions.go` L96–100, L340–341; `store.CreateUserSession`). `RevokeUserSession` revokes only the session (`user_identity.go` L409–412), and Android sign-out calls it (android `ClientSessionApi.kt` L290–304). `ActivePushAuthority`; `RevokeUserClient`; `RevokeAllUserAuthority`; `lockActiveUserAuthority` | `backend/store/user_identity.go`, `beta_write_authority.go` |
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
  - Disable, re-pin and `reset-identity` clear **all** wake state in the same commit.
- **Provider opt-in:** `amux provide --direct-wake` (`AMUX_PROVIDER_DIRECT_WAKE=1` / `direct-wake = true`). It advertises only while the policy is enabled with matching pins; same 30 s recheck.
- **No other provider features are needed.** `sessions`, `runtime-events` and `direct-enroll-v1` are **not** required. Wake-only discovery and registration work without them (§6.1, tested in §8).

### 4.2 Direct API (GET/POST only, so current CORS is unchanged; bearer access token; `read`; no new permission)

JSON bodies. Unknown fields and query parameters answer `400`. route_id is never in a path or query. All grant kinds opt in without re-enrollment.

| Route | Body | Result |
|---|---|---|
| `POST /direct/v1/wake/subscribe` | `{route_id, activation_code}` | This grant's subscription + an `activate` outbox item, in one commit. `200 {route_id, state:"activating"\|"active"\|"failed", failure?, created_at}`. Same grant + route + code is idempotent. `409 wake_route_conflict` if another grant's subscription holds the route (it cannot be active without its code). `409 wake_disabled`. `409 capacity_exhausted` (more than 4 per grant). |
| `POST /direct/v1/wake/unsubscribe` | `{route_id}` | `200 {removed}`. Idempotent. Deletion + `deactivate` item in one commit. |
| `POST /direct/v1/wake/subscriptions` | `{}` | this grant's `[{route_id, state, failure?, created_at, watches:[{session_id}]}]`. POST, so route ids never sit in a GET URL. |
| `POST /direct/v1/wake/watch` | `{route_id, session_id, watch:bool}` | Adding requires the session in scope now (`404 not_found`) and records its **current incarnation** (`SessionIdentities`). Removing never needs access. At most 64. |
| `POST /direct/v1/wake/events` | `{route_id, after_seq?, limit:1..64}` | §4.4 |
| `POST /direct/v1/wake/ack` | `{route_id, wake_ids:[≤64]}` | `200 {acked}`. Idempotent. |

`GET /direct/v1/capabilities` gains `wake:{v:1, enabled:bool, degraded:bool}`, always serialized by new servers. Old servers answer a 404 with no JSON code. A CORS preflight test covers each new route.

### 4.3 Detection: baseline, monotonic high-water marks, incarnations

- **Session set.** The union of in-scope sessions over subscriptions: valid id, not console, not effectively archived.
- **Baseline and mark.** When a session enters a subscription's set, one commit records:
  - `baseline[sub][sid] = Head` (from `History`);
  - `mark[sid] = max(mark[sid], Head)`;
  - `incarnation[sid]` (from `SessionIdentities`).

  Only events with ordinal > `baseline[sub][sid]` can produce wakes for that subscription.
- **Mark.** `mark[sid]` is a durable high-water mark that **only goes up**. It advances only in the same commit as the records produced by the events up to it.
  - Tails resume **from the mark**: on idle → live, on restart, and on tail reconnect. A fast idle → permission inside one poll interval is therefore caught.
  - **Dedupe rests on the mark.** An event at or below it is never re-read, so ack or expiry cannot re-queue it. Identity tombstones (24 h) are defense in depth only.
- **Incarnation.** If `SessionIdentities` reports a different incarnation for `sid`, or the session is gone, or `Head` < `mark`:
  - the session is re-baselined at the new head (mark reset to it, in one commit);
  - watches recorded for the old incarnation are **deleted**, so a reused id never inherits a watch;
  - open permission records of the old incarnation become `stale`.
- **Identities.**
  - A permission's identity is `sha256(sid, incarnation, "permission_request", item_id, request_id, runtime_generation)`, length-prefixed.
  - A turn end's identity is `sha256(sid, incarnation, "turn_end", ordinal)`.
- **Permission state.**
  - `open` → `resolved` when a matching `permission_resolved` (same request_id + runtime_generation) is seen.
  - `open` → `stale` when a later event in the session carries a different non-empty `runtime_generation`, the session goes idle, or the session is re-baselined.
  - Answering is still validated by the runtime.
- **No starvation.** Up to 64 live sessions are tailed with `Stream(mark)`.
  - All others are polled round-robin with `History`, paging back to the mark (≤ 8 pages per turn; continued next turn, never skipping).
  - Each in-set session is checked at least every `max(2 s, ⌈N/16⌉×2 s)`.
  - If any session's lag exceeds 60 s, `degraded:true` appears in `wake status` and capabilities.

### 4.4 Atomic durability and fetch

**Atomic durability (decision: option (a)).**
- Policy, subscriptions, watches, records, marks, baselines, incarnations, tombstones and outbox state all live in `storeState.Wake` in `grants.json`. Every change is one `commitLocked`.
- Grant revoke or expiry deletes the grant's wake state in the same commit as the revocation or prune.
- `errNotCommitted` reverts every wake mutation of that operation in memory.
- An ambiguous commit degrades the authority (wake ops refuse until restart).
- **Old-binary rewrite.** It drops `Wake`. The new binary then sees no policy, so wake is **off** with no subscriptions. Re-enable gets a fresh nonce and key, so no earlier record, grant_ref or outbox item can match.

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
  Items that fail are tombstoned in the same commit.
- **Unknown routes.** An unknown route, or another grant's, returns `200 {"items":[]}`.
- **Content and actions.**
  - **Content** comes only from `history`/`stream`.
  - **Answering** uses the existing `permission` action, which requires `approve`.
  - **Allow/Deny** is shown only for `open` items when capabilities `verbs` has `permission`.
- **Ack.** Ack tombstones records.

### 4.5 Leased, retriable outbox (no pre-send loss)

Item kinds: `wake`, `activate`, `deactivate`. Durable states: `pending | delivered | gone | expired`.

- **Leasing.** `wake_lease {max≤16}` is a long-poll under 15 s. It gives `lease_id` + items. Leases are in memory only, 30 s.
- **Settling.** `wake_settle {lease_id, results}` is called **after** the Harness answer.
  - `accepted` / `no_recipient` → `delivered`.
  - `route_gone` → `gone`, subscription `failed:route_gone`.
  - `rate_limited` / `unavailable` / `busy`, a frame timeout or a disconnect → back to `pending`, with backoff from 2 s to 60 s.
- **Exactly-once.** A re-send has the same `wake_id`, so Harness's ledger delivers it once (§6.3).
- **Deadlines.**
  - A wake stops retrying 900 s after creation (`expired`); the record stays fetchable.
  - `activate` expires with the code TTL (10 min) → `failed:activation_expired`.
  - `deactivate` retries up to 24 h; route sync converges it anyway.
- **Revocation race.** Revoke, unsubscribe or a policy change deletes items in the same commit. A later settle for a missing item is ignored. At most one wake that was already submitted can be delivered; its fetch answers 401 or is empty.

### 4.6 Route sync

On every registration and every 30 s, the provider sends `direct-wake-routes {serverId, routes:[{routeId, grantRef}], part, final}` (≤ 512 per part). It is the complete set of this host's active subscriptions. Harness applies it **only to routes with `provider_id = conn.providerID` and `server_id = serverId`**:

- An active route absent from two consecutive complete syncs → revoked (`deactivated`).
- A `grantRef` mismatch → ignored for that route.
- Unknown or foreign ids → ignored, with no mutation.

## 5. Wire

### 5.1 harnessproto `direct-wake-v1` (additive; goldens)

| Frame | Direction | Body |
|---|---|---|
| `registered.features` | orch → prov | accepted features echoed. Wake frames are sent only after the echo. |
| `direct-wake-activate` | prov → orch | `{reqId, directWake:{serverId, routeId, activationCode, grantRef, requester?}}` |
| `direct-wake` | prov → orch | `{reqId, directWake:{serverId, wakes:[{routeId, wakeId, grantRef}] ≤16}}` |
| `direct-wake-routes` | prov → orch | `{reqId, directWake:{serverId, routes:[…] ≤512, part, final}}` |
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
  "title":"Activity on a direct machine", "body":"Open the app to see what needs you.",
  "harness_session_id":"…","harness_client_id":"…","user_id":"…","origin":"…",
  "authorization_generation":"…","expires_at":"…" }
```

- **No `server_id`** (finding E): the phone maps route_id → backend locally.
- No kind, session, request, generation or item.
- The beta authority envelope is the same one relay pushes carry, so the existing Android gate applies.

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

- **`device.direct_wake.register`** (`server_id`; there is **no provider_id field**).
  - **Provider resolution.** Harness resolves the provider from the live index: exactly one owned provider live with `directWake.serverId == server_id`. None answers `wake_unavailable`; more than one answers `wake_ambiguous`.
  - **Checks:**
    - the caller owns that provider (live row);
    - at least one device row exists for (U, C);
    - the write runs under `lockActiveUserAuthority`;
    - at most 8 active routes per client.
  - **Existing route.** An existing active route for (U, C, P, S) is revoked (`replaced`).
  - **Result:** `{route_id, activation_code, expires_at}`.
- **`device.direct_wake.unregister`** (`route_id`): owner- and client-scoped, idempotent.
- **`device.direct_wake.list`**: the caller client's routes `{route_id, server_id, state:"pending"|"active", created_at}`. Codes are never returned.

### 6.2 Validating provider input: no cross-tenant mutation

The provider is always `conn.providerID`. Answers for foreign and nonexistent routes are **indistinguishable**.

| Condition | Answer | Mutation |
|---|---|---|
| Frame `serverId` ≠ the connection's current negotiated serverId, or the connection is not in the live index | frame `error:"unavailable"` | none |
| Route unknown, or `provider_id ≠ conn.providerID`, or `server_id ≠ serverId` | `route_gone` | **none** |
| Route pending (wake), or `grantRef ≠ route.grant_ref` | `route_gone` | none |
| The route's own binding is invalid from server state: provider owner ≠ `route.user_id`; user inactive; generation ≠ route's; client revoked | `route_gone` | revoke **this** route. Allowed because it belongs to `conn.providerID` and the reason is server state, never frame data. |
| Activation: hash mismatch | `route_gone` | none (a forged code never burns the real one) |
| Activation: hash match but expired or consumed | `route_gone` | consume |
| Activation: requester ≠ `route.user_id` | `route_gone` | consume and revoke (`requester_mismatch`) |
| Rate: 30/min per route, 120/min per provider | `rate_limited` | — |
| Transient DB error | `unavailable` | — |

### 6.3 Delivery: one client; dedupe claimed only after acceptance

- **A new `push.DirectWakeDelivery`**, separate from relay `Notify` (finding A).
  - It reuses only the `Sender` interface and the authority-envelope builder.
  - Recipients are `devices WHERE owner_user_id = route.user_id AND client_id = route.client_id`, and nobody else.
- **The send-boundary query.** One SQL statement, run immediately before each `Send`, returns the device token together with the envelope. It joins:
  - `direct_wake_routes` (active, not revoked);
  - `devices` (owner + client match);
  - `user_clients` (not revoked, kind android);
  - `app_users` (active, `authorization_generation = route.authorization_generation`);
  - the newest live `user_sessions` row for C (not revoked, not expired, current generation).

  This is `ActivePushAuthority` inlined in the same statement.
- **Dedupe ledger.**
  1. `INSERT … device_push_dedupe(device, "dw1:"+sha256(provider_id, server_id, wake_id)) ON CONFLICT DO NOTHING`. A conflict means the wake was already delivered or is in flight: skip that device.
  2. Then `Send`.
  3. **On a transient error (429, 5xx, timeout), the claim row is deleted** and the result is `unavailable`, so the host retries.
  4. On success, or on `ErrTokenGone` (token pruned, as today), the claim stays.
- **Overall result.**
  - `accepted` if every recipient device was accepted by FCM or was already claimed.
  - `unavailable` if any had a transient failure. The retry re-sends only to the unclaimed devices.
  - `no_recipient` if the client has no device or no live session. This is terminal for that wake; the record stays fetchable.
- **The relay `Notify` ledger behavior is unchanged.** It is out of scope, and the review noted it as pre-existing.

### 6.4 Coexistence: phone-side, per event; Harness suppresses nothing

- **Relay behavior is unchanged.** The relay keeps pushing relay cards (all owner devices, existing `device.watch`). Direct wakes are delivered independently to the opted-in client. **Harness never suppresses either path**: it cannot know grant scope.
- **Exact permission correlation (phone).** A relay card and a direct item correlate **only if every one of these holds**. Otherwise they are independent cards.
  1. **Verified provider ↔ server pairing.** The relay push's `scope=daemon`, `id` (provider_id) equals the provider_id of the phone's **active** route for this direct backend (`device.direct_wake.list`).
     - Harness resolved that provider from a live connection advertising this `server_id` (§6.1).
     - The phone verified that same `server_id` through the direct signed hello at enrollment.
     - Both halves of the pairing are therefore verified, not just raw ids.
  2. **Current session incarnation.** The direct item's `incarnation` is the session's current incarnation: fetch returns only current-incarnation items (§4.4), and the incarnation travels with the item.
     - A relay card can match only when its `runtime_generation` and `item_id` equal those of that current-incarnation item.
     - These are producer occurrence ids that change across runtime and session incarnations, so a reused session id from an earlier incarnation cannot match.
  3. **Equal producer tuple.** `session_id`, `item_id`, `request_id` and `runtime_generation` are equal. None of them may be empty: a generation-free relay row never correlates.
- **When a correlated item is covered by direct.** "Covered" means the direct fetch returned the item, in scope and `open`.
  - The card is **direct-owned**: content from direct, actions via the direct `permission` verb only.
  - **An opted-in direct card never routes an action via relay.** Without `approve`, the direct-owned card shows no Allow/Deny and the relay card is kept (rule below), so no relay action replaces a direct one.
- **Out-of-scope and uncorrelated cloud cards are never dropped.** A relay card is replaced only by a direct card that covers the same exact tuple. In every other case the relay card stands with its existing semantics:
  - the session is outside the grant's scope;
  - the direct fetch failed or timed out;
  - there is no active route;
  - the pairing or incarnation does not match;
  - the grant lacks `approve`.
- **Timing (stated bounds, no zero-duplicate promise).**
  - If the relay card is drawn first, the phone replaces it under the same collapse key once the direct fetch covers the tuple.
  - If the direct card is drawn first, a later relay push for a covered key is dropped.
  - A duplicate can remain visible for at most one direct fetch round-trip, or permanently only if the phone lacks the provider ↔ server mapping.
- **turn_end.** The relay `turn_end` has no ordinal: its request_id is `turn:<scope>:<session>:<unix>`. A relay-watched and direct-watched turn end therefore **may produce two cards**. This is documented. The phone may collapse on `(provider_id, session_id)` within a short window, which is Android's choice. A session watched only on the direct side always gets its turn end (no suppression).
- **Scoped grants.** Direct wakes cover only the grant's scope and its own watches. Relay coverage for everything else is unchanged.

### 6.5 Fences: revocation, logout, removal, rotation, late work

| Event | Effect | Late work |
|---|---|---|
| **Explicit Harness sign-out** on client C (`RevokeUserSession` of a session of C, i.e. Android `DELETE /api/auth/session`) | **all** of C's routes revoked in the same transaction (`signed_out`). A later login never revives them: re-opt-in means register + activate. | route state is checked in the send-boundary query |
| Session **expiry** (normal ≤ 8 h renewal), no sign-out | the route survives. Delivery returns `no_recipient` while C has no live session, and resumes with C's next session (same client credential, user and generation). | — |
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
  - disconnect, timeout, `busy`, `rate_limited` and `unavailable` → re-delivered exactly once (same wake_id);
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
  - cascades from `RevokeUserSession` (**logout → login does not revive**), `RevokeUserClient` and `RevokeAllUserAuthority`;
  - expiry + renewal keeps the route;
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
  - send-boundary recheck after a mid-flight sign-out or generation bump;
  - payload key-set allowlist (no server_id);
  - no-device and no-session → `no_recipient`.
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
