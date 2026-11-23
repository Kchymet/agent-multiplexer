# Direct access to the console (design)

Status: proposal for sign-off. Nothing here is implemented yet.

Goal: let the Harness web client and the Android app see the console's
follow-up inbox and workgroup summary (Parts 1 and 2), and let them steer
the console, over `amux direct`. Direct access does not grow beyond what the
host operator grants explicitly.

## 1. Why the console is excluded today

- **#273 (15fcf0e), direct S1.** This added `ConsoleID`, `isConsole` and
  `inScope`'s early `return false` (`internal/direct/sessions.go`). It also
  added the `ValidateResource` refusal "the console cannot be granted over
  direct access" (`authority.go`). The reason, from the code comment and
  `docs/direct-access.md`: the console "carries operator and account powers,
  so S1 direct access never exposes it … under any grant, including
  all_sessions". The doc adds: "An explicit console grant would be a
  separately reviewed feature." This is that review.
- **#278 (96811de), cloud-assisted enrollment.** The ceiling is "read or
  steer, `--all-sessions` or `--workgroup`, with the console excluded and no
  implicit all-host scope". `withinCeiling` (`cloud.go`) depends on "the
  console is never valid".
- **#296 (95ed43c), workflow verbs.** Step 5 of the check order hard-codes
  "console never in scope" before every `Apply`. `actions.go` refuses the
  console as a creation parent.
- **#313 (2922c17), wake-ups.** `wake_service.go` and `wake_store.go` keep
  the console out of wake detection and snapshots.

**The root reason is laundering.** Direct's own surface fails closed for
delete, move, repo tracking, ceiling changes, accounts and host
administration (`direct-access.md`, "Everything else fails closed"). The
console passes every `Console` rule (`docs/workgroup-permissions.md`). A
free-text prompt to the console therefore turns into exactly those
operations, carried out by an LLM. A `steer` grant on the console would
silently undo every per-verb boundary #296 drew.

## 2. Threat model

**What changes.** Today the worst a stolen direct credential can do is the
verbs its grant lists, in its scope. Console steering adds "anything the
console may do on any workgroup". Inbox and summary reads add a global,
cross-workgroup view. Item titles and details may name repos, PRs, risks and
decisions.

| Threat | Today | With console access | Required control |
| --- | --- | --- | --- |
| Stolen, unlocked phone | Grant's verbs | Drive every workgroup via the console | Step-up with user verification (biometric or passkey) for each elevation; short elevation lease |
| Leaked refresh or access token (bearer, not sender-constrained) | Same as phone | Same, from anywhere on the route | Step-up key is non-exportable (Keystore or passkey), so a token alone can never steer the console. Refresh reuse detection already revokes the grant |
| XSS in the Harness web client (`tokenStore.ts`: "script running on this origin can read them") | Grant's verbs | Inbox leak; triage changes | Token theft gives read/triage at most. Steering still needs a WebAuthn user-verification ceremony, which script cannot complete silently. Accepted residual risk: a passkey proves presence, not what the user saw (no what-you-see-is-what-you-sign on the web) |
| Malicious web origin | Refused by the exact `--allow-origin` and no credentialed CORS | Same | Unchanged. Assertions are checked against `rpIdHash` and `clientDataJSON.origin` ∈ allowed origins, so another origin's passkey cannot elevate |
| Cloud relay or provider compromise | Can mint codes within the ceiling for the pinned owner | Could mint console-capable codes | `console_steer` is never in a cloud ceiling. `console_read` only when named explicitly. Wake-ups stay content-free |
| Prompt injection via items (repo, PR or comment text → member → coordinator → console) | n/a | Item text is shown on the phone, and a tap could act on it | Items are untrusted data. They render as plain text, links are https-only with the host shown, and nothing runs from item text. Item actions take an item id and send structured parameters the user confirms, never "prompt the console with the item" |
| Replay | Idempotency keys; one-time codes | A step-up assertion replayed | Challenges are single-use, live 60 s, and are bound to grant, server id and nonce. WebAuthn signCount is checked. The elevation lease belongs to the grant and ends on revoke |

## 3. Options

**(a) A separate permission tier.** *Adopt.* This follows the `accounts`
precedent (#296 and the "Machine accounts" section of `direct-access.md`):
an additive permission, `all_sessions` only, never implied, and stored and
copied on refresh. It is inert in older binaries: their `loadState` is
lenient and `Admit` is an exact match. Two permissions, not one, because
reading and steering carry very different risks.

**(b) Step-up confirmation.** *Adopt it for steering, not for reading.*
Confirming on the host defeats the purpose when the operator is away.
Instead, the client registers a **step-up key** at enrollment:

- **Web:** a WebAuthn passkey with user verification required (`uv`).
- **Android:** a Keystore EC P-256 key with
  `setUserAuthenticationRequired` and BiometricPrompt via a `CryptoObject`.

The host stores the public key on the grant and verifies ES256 assertions
itself, with no new dependency beyond `crypto/ecdsa`. Host-side TUI
confirmation stays a possible later fallback, not v1.

**(c) Read-only vs full steer.** *Both, as separate tiers.* Most of the value
is read-only: seeing what needs the operator from the phone.

Most console-level downward actions do not need the console at all, because
existing verbs already cover them:

- "Steer coordinator from item" is `prompt` on the coordinator (`steer`).
- "Create workgroup from item" is `new-workgroup` (`create` +
  `all_sessions`).

So the client composes them from existing verbs. Only free-text
conversation with the console agent is genuinely new authority.

**(d) Per-action allowlists.** *Adopt at the protocol level.* Console routes
get exact field allowlists. Item verbs are a closed set: `acknowledge`,
`dismiss`, `done`, `reopen`. They are bookkeeping, reversible (`reopen`) and
never delete. A per-prompt allowlist of what the console may do is not
enforceable: the console is an LLM with `Console` rules. The step-up and the
lease are the boundary.

**(e) Audit log.** *Adopt.* Every console route call over direct is
appended to `console-audit.jsonl` in the direct state dir (0600, rotated at
10 MiB). Each entry records:

- the time, grant id and label;
- the route and verb, and the item id;
- the prompt text, capped at 4 KiB;
- the step-up assertion id and lease id;
- the outcome.

`amux direct audit [--client <id>] [--json]` reads it. The daemon also tags
the console's injected prompt with provenance metadata (`via: direct`,
client label) in event metadata, never in the prompt text.

**(f) Revocation.** *Reuse the existing mechanism.* `amux direct clients
revoke` already cancels in-flight leases and waits for streams to finish.
Revocation now also ends the elevation lease and drops the step-up key.
`clients list` shows the console permissions and the time of the last
step-up.

**Cloud ceilings (`cloud.go`).** `ValidatePerms` accepts the new names, but:

- `cloud enable` refuses `console_steer` outright.
- `console_read` is admitted only when named in `--permissions` together
  with `--all-sessions`.
- `withinCeiling` keeps "console never valid" for resource scopes. The
  console is reached by permission, never by `--workgroup console`, which
  stays refused.

## 4. Recommendation and default

1. **`console_read`** (needs `all_sessions`): workgroup summary, inbox list
   and watch, and the item verbs. It does not include the console
   transcript.
2. **`console_steer`** (needs `all_sessions` and `console_read`, plus a step-up
   key registered at token redemption):
   - The console appears in inventory (`role: "console"`) for this grant
     only.
   - History and stream are readable.
   - `stop` works without elevation.
   - `prompt`, `interject` and `permission` answers need an **elevation
     lease**. One verified assertion opens it for 5 minutes (configurable
     only downward), for this grant only.
   - `manage` and `create` verbs never apply to the console.
3. **Default: off.** Neither permission is implied by `all_sessions` or any
   other permission, and there is no `all` alias. They are granted with
   `amux direct enroll --permissions read,console_read[,console_steer]
   --all-sessions` (QR or code). Enrollment fails if `console_steer` is
   requested and the redeeming client sends no step-up key.
4. **Cloud:**
   - `console_read` only if the ceiling names it.
   - `console_steer` never in v1. Raising it later is a separate review.
5. **Wake-ups:** none for the console in v1. A content-free
   `console_inbox` wake for `decision_needed` items is a follow-up, using
   the #313 contract.
6. **Audit and revocation** as in 3(e) and 3(f).

## 5. Protocol sketch (additive, `/direct/v1`)

| Route | Permission | Notes |
| --- | --- | --- |
| `GET /console/summary` | console_read | Part 2 buckets: `needs_you`, `running`, `waiting_review`, `done`. Each entry has a workgroup id and title, counts and PR numbers, from an allowlisted projection (no paths or branches) |
| `GET /console/inbox?state=&cursor=&limit=` | console_read | Part 1 items: `id`, `kind`, `title`, `detail` (capped), `source` (session or workgroup id), `links` (https only), `prs`, `suggested_action` (an enum plus structured params), `state`, `rev`, timestamps |
| `GET /console/watch` (WebSocket) | console_read | Pushes summary and inbox revisions, like `sessions/watch` |
| `POST /console/inbox/{id}/actions` | console_read | `{verb, if_rev, idem_key}`. `verb` is `acknowledge`, `dismiss`, `done` or `reopen`. A stale `if_rev` gets 409 `conflict` |
| `POST /console/step-up/challenge` | console_steer | Returns `{challenge, expires_at}`: single-use, 60 s |
| `POST /console/step-up` | console_steer | Takes `{challenge_id, assertion}` (a WebAuthn `get` result, or `{sig}` for the Keystore key). Returns `{lease_expires_at}` |
| Existing session routes with `sid=console` | console_steer | `prompt`, `interject` and `permission` without a live lease get 403 `step_up_required`, with `WWW-Authenticate: … error="insufficient_user_authentication"` (RFC 9470) |

- **Token endpoint.** Code and ticket redemption accept an optional
  `step_up_key`: `{alg:"ES256", kind:"webauthn"|"keystore", public_key
  (SPKI b64url), credential_id?, rp_id?}`. The host stores it on the grant.
  Refresh copies the grant and never changes the key.
- **Capabilities.** A new `console` object:
  `{read, steer, step_up: "webauthn"|"keystore"|null, lease_expires_at}`.
  When the object is absent, the server predates this feature.
- **Old clients.** They send no `step_up_key`, so they can never hold
  `console_steer` and never see the console row. They ignore unknown
  permissions and capability fields.
- **Old hosts.**
  - They 404 the `/console/*` routes; clients treat that like absent
    capabilities.
  - They refuse the new permission names at enrollment.
  - Stored names stay inert (`TestUnknownStoredPermissionIsInert`).
- **Fixtures.** A new `docs/direct/fixtures/console-v1` set with
  `SHA256SUMS`. The existing sets are unchanged.
- **Errors.** `404 not_found` stays indistinguishable for grants without
  the permission on console session routes, as today.

**The check order** for console routes is:

1. credential;
2. permission (403 + `scope=`);
3. `all_sessions`;
4. field allowlist;
5. idempotency key;
6. lease (steer verbs only);
7. fresh daemon read (Part 1/2 ops, or Snapshot for the console row);
8. Apply;
9. audit append.

Audit is written before the response. If the audit write fails, the
response is a failure and nothing further is admitted until restart, the
same rule as ambiguous writes.

**Web client (harness, `frontend/src/direct/`, `components/rail/`).**

- **Wire and API.** `wire.ts` and `engine.ts` gain the routes and the
  `console` capability.
- **Inbox pane.** A new `ConsoleInboxPane` sits beside `NeedsYouPane.tsx`.
  The summary buckets come first, then the items, filtered by state.
- **Item card.** Kind chip, title, plain-text detail, source link, PR
  chips. Action buttons: acknowledge, dismiss, done or reopen.
  - "Steer coordinator" opens the existing steering composer on the
    coordinator.
  - "Create workgroup" opens `DirectCreateModals` prefilled, and the user
    confirms the fields.
- **Console chat.** It appears only with `steer`. It reuses
  `useDirectTranscript` and `useDirectSteering`. The composer is locked
  until a "Unlock console (5 min)" passkey ceremony succeeds, and shows the
  lease countdown.
- **Enrollment.** Enrollment in `auth.ts` and `deviceTicket.ts` creates the
  passkey when the code carries `console_steer` (the bundle shows the
  permissions).
- **Tests.** vitest against `testing/fakeServer.ts`, fixture conformance,
  and a `ci/e2e/direct` driver run against a real host.

**Android (harness-android).**

- **Network.** `core/network/.../direct/DirectApi.kt` and `DirectWire.kt`
  gain the routes. A `ConsoleRepository` wraps them.
- **Step-up key.** `DirectVault` generates the Keystore key at redemption
  (`DirectTokenApi`).
- **UI.** A `ConsoleInboxScreen` (Compose) is reachable from the backend's
  home when `capabilities.console.read` is true, with the same cards and
  actions as the web. A `ConsoleChatScreen` uses BiometricPrompt to sign
  the challenge.
- **Tests.** JVM tests against the shared fixtures (`DirectFixturesTest`
  pattern) and ViewModel tests for lease and permission states.

## 6. Implementation plan

**agent-multiplexer.** Stacked on the Part 2 branch; it needs Part 1's item
store and Part 2's summary and inbox daemon ops.

1. **Permissions and enrollment.**
   - `PermConsoleRead` and `PermConsoleSteer` in `wire.go`.
   - `all_sessions` and dependency checks in `ValidatePerms` and
     `ValidateResource`.
   - The cloud ceiling refuses steer and requires read to be explicit.
   - `step_up_key` on redemption, the store field and migration.
   - `clients list` shows the new columns.
   - Tests:
     - every existing grant type is denied all console routes with zero
       backend calls;
     - unknown permissions stay inert;
     - cloud refusal;
     - steer without a key is refused;
     - refresh preserves the key.
2. **Read routes.**
   - Summary, inbox, watch and item verbs over hostbackend into the
     Part 1/2 ops.
   - Projection allowlist, audit log and `amux direct audit`.
   - Tests:
     - leak checks (no paths or branches);
     - `if_rev` conflicts;
     - idempotent replay;
     - audit written before the response;
     - audit-failure fail-closed.
3. **Steer.**
   - The step-up challenge and verifier (WebAuthn ES256 and Keystore
     ES256) and the elevation lease.
   - The console becomes in scope only for `console_steer` grants;
     `inScope`, `wake_*` and `actions.go` keep the exclusion for everyone
     else.
   - Provenance metadata on prompts.
   - Tests:
     - replayed or expired challenge;
     - wrong origin or rpId;
     - signCount regression;
     - lease expiry mid-session;
     - revoke ends the lease;
     - `manage` and `create` still refuse the console;
     - an e2e run against a live daemon.
   - Docs: update `direct-access.md`, `direct-cloud-enrollment.md` and the
     fixtures.

**harness (web).**

4. Wire, engine and capability support, plus the read-only inbox and
   summary pane and item actions (it can merge once PR 2 lands).
5. Passkey enrollment, the unlock flow and the console chat (after PR 3).

**harness-android.**

6. Wire, repository and the inbox screen (after PR 2).
7. The Keystore step-up key, BiometricPrompt unlock and console chat (after
   PR 3).

PRs 1–3 land in order. The client PRs follow their host dependency.
Read-only is useful on its own, so it can ship before steering.
