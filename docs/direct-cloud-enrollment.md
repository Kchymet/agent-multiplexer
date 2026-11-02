# Cloud-assisted direct enrollment

Cloud-assisted enrollment lets a cloud application (Harness) broker discovery
and setup for [direct access](direct-access.md). The direct server still issues
its own credentials, and the cloud stays out of the data path. It is **off by
default**. Turning it on takes two operator choices: enable the policy on the
direct server, and opt the provider in.

## How it works

1. The client generates an RFC 7636 verifier and sends only its S256 challenge
   to Harness.
2. Harness authenticates the user and checks that they own this provider (from
   its live database, never from the request body). It then sends a typed
   `direct-enroll` frame over the existing provider connection.
3. `amux provide` adds its own registered provider id and passes the request to
   the direct server's host-only admin socket as one typed `issue_cloud_code`
   call.
4. The direct server checks the operator's policy and mints a 5-minute,
   single-use code bound to the challenge. It returns an ordinary paste bundle
   (`amux-direct:1:…`, format unchanged).
5. The client verifies the endpoint itself (trusted TLS and the signed hello
   naming `server_id`). It then redeems the code at `POST /direct/v1/token`
   with `code_verifier`.

Harness never sees the verifier, any access or refresh token, or any session
traffic, and it never dials the advertised endpoints.

## Device tickets (connect another device by QR)

A device ticket lets an owner, signed in to Harness in a browser, connect
**another** device (typically a phone shown a QR code) without the browser or
Harness ever holding that device's verifier or tokens, and without the phone
signing in to Harness.

1. The browser sends the ordinary Harness enrollment request with
   `handoff: "device_ticket"` and **no** `code_challenge`. Every existing
   check applies: owner-only, live provider, matching `server_id`, the host's
   pins, ceiling, capacity, rate limit and issue-key idempotency.
2. The direct server mints the code **without** a challenge, marked as a
   ticket, and returns a v2 bundle: `amux-direct:2:` +
   base64url(`{v:2, server_id, endpoints, code, expires_at, claim:"pkce"}`).
   Clients that only know v1 refuse it.
3. The receiving device verifies the endpoint itself (trusted TLS, signed
   hello naming `server_id`), generates its **own** RFC 7636 verifier, and
   claims the ticket at the host: `POST /direct/v1/enroll/claim` (form body
   `code`, `code_challenge`, `code_challenge_method=S256`).
4. It then redeems with the unchanged cloud grant at `POST /direct/v1/token`
   with its `code_verifier`, receiving its own grant and tokens.

**Ticket authority.** Until it is claimed, a ticket is registration authority
for exactly one new grant at its minted scope, for whoever claims it first —
including the browser that displayed it. It is single-use, expires with the
cloud code TTL (5 minutes), is bound to this host (`server_id`), to the
owner/provider pins and to the policy generation, and can never reach an
existing grant or refresh token.

**Claim rules.**

| Situation | Result |
| --- | --- |
| Live unclaimed ticket, valid S256 challenge | Challenge stored and committed durably, then `200 {claimed:true, expires_at}` |
| Same challenge again (lost answer) | `200`, nothing written |
| A different challenge after a claim | `invalid_grant`, nothing changed |
| Not a ticket (manual or PKCE code), consumed, expired, policy changed or disabled | `invalid_grant` |
| Malformed challenge or method other than `S256` | `invalid_request` |
| Store write not confirmed | `temporarily_unavailable`; retry with the same challenge |

Failed claims count toward the same per-source limit as the token endpoint.
Parameters in the URL are refused.

**Redeeming a ticket.** An unclaimed ticket at the token endpoint is
`invalid_request` and is not consumed, with or without a `code_verifier`.
After a claim, the cloud PKCE rules above apply unchanged: a wrong verifier
consumes it, and a policy change since issuance refuses it.

**Ordinary cloud codes are unchanged.** A request without `handoff` still
requires an S256 challenge; a ticket request carrying one, or an unknown
`handoff`, is refused. An older host that does not know `handoff` refuses the
missing challenge (`invalid_challenge`). Hosts that mint tickets advertise
`capabilities.direct.handoff: true`.

Fixtures: `docs/direct/fixtures/ticket-v1` (own `SHA256SUMS`; the `v1` and
`cloud-v1` manifests are unchanged and pinned).

## Operator guide

**Setup.** Register the host with Harness, then turn on issuance with only the
ceiling, then opt the provider in:

```
amux provide ...                        # running, until `amux doctor` shows "registered" and an owner
amux direct cloud enable --permissions read,steer --all-sessions
# restart the provider opted in (stop the running one first: one provider per host):
amux provide --direct-enroll            # or AMUX_PROVIDER_DIRECT_ENROLL=1, or direct-enroll = true
```

The feature is advertised at a registration, so after `enable` the provider
must register again with the opt-in. Restart it, or restart its service after
setting `direct-enroll = true`.

`enable` prints what it pinned before applying it:

```
resolved from this host's provider registration (registered, registered 2m0s ago):
  Harness   harness.example.com:7443
  account   usr_01HZXK5Q8M (you@example.com)
  provider  prov-42
cloud enrollment: enabled (generation 3)
  ...
```

**Full syntax:**

```
amux direct cloud enable --permissions read[,steer] (--all-sessions | --workgroup <root-id>...) \
    [--owner <harness-user-id>] [--provider <provider-id>] \
    [--max-outstanding 3] [--max-per-hour 10]
amux direct cloud status [--json]
amux direct cloud disable
```

**The flags:**

- **The ceiling** has no implicit default. `--all-sessions` covers ordinary
  sessions and never the amux console. `--workgroup console` is refused.
- **`--owner` and `--provider` are optional.** An omitted pin comes from this
  host's **current** provider registration. That is the `owner` and
  `providerId` Harness returned in `registered` to this host's
  token-authenticated registration, as recorded in the `amux provide` status
  file and shown by `amux doctor`. It is used only while that provider is
  registered right now: state `registered` and its process alive, the same
  liveness `amux doctor` checks. A dialing, disconnected, stopped or dead
  provider's last snapshot is refused rather than trusted for some period.
  `amux provide` holds a lock on the status file, so a second provider on the
  host fails at startup (`another amux provide is already running on this host
  (pid N)`) instead of making the registration ambiguous.
  Harness fills `owner` from its live provider row, so the defaults are exactly
  the requester and provider it will vouch for. The email is shown only so you
  can recognize the account. It is never used as an identity.
- **Supplying both flags is the advanced override.** They are pinned verbatim
  and the registration is not needed. `enable` prints a note when they differ
  from it.
- **The pins are explicit host configuration.** They are set only by this
  host-only command, never from a relayed request. Re-running `enable` re-pins
  and starts a new generation.

**Resolution errors.** `enable` changes nothing and names the fix:

| Message starts with | Cause | Fix |
| --- | --- | --- |
| `no Harness provider registration on this host` | `amux provide` has never run here | Run `amux provide` against Harness |
| `the provider's last registration … was rejected` | Token revoked or invalid | Fix the provider registration (`amux doctor`) |
| `the provider is not registered with Harness right now (state …)` | Dialing, reconnecting after a drop (`disconnected`), or stopped | Start `amux provide` and wait for `amux doctor` to show `registered` |
| `the provider recorded as registered (pid N) is no longer running` | The provider died without recording a stop (SIGKILL, reboot) | Start `amux provide` again |
| `Harness reported no owner for provider …` | Shared or unowned provider, or a Harness that predates owner reporting | Register the provider to your own account, or upgrade Harness and restart `amux provide` |
| `--provider X is not this host's registered provider` | Only `--provider` given, for a different provider | Give both `--owner` and `--provider`, or drop the flag |
| `--owner X is not the registered owner` | Only `--owner` given, for a different account | Give both flags, or drop the flag |

`amux direct cloud status` warns when enabled pins no longer match the latest
registration (Harness reassigned or recreated the provider). Re-run `enable` to
re-pin.

**Getting the ids by hand (explicit pins only).** The provider id is
`providerId` in `amux doctor`. The Harness user id is the `id` field of
`GET /api/me` on the Harness origin, opened in a browser that is already signed
in.

**What you are trusting:** enabling issuance means trusting the pinned owner,
through Harness, to mint enrollment codes up to the ceiling. PKCE does not
protect against a compromised but authorized issuer that requests codes for a
verifier it holds itself. Every cloud grant records its provenance:
`amux direct clients list` shows `cloud:<requester>` whatever `client_label` the
client chose.

**Lifecycle:**

- **Every policy change starts a new generation.** That includes enable,
  re-pin, a lowered ceiling and disable, and every outstanding cloud code
  becomes unredeemable.
- **Grants already redeemed are not revoked by a policy change.** Revoke them
  explicitly with `amux direct clients revoke`.
- **`amux direct reset-identity` disables cloud issuance.** Re-enable it
  deliberately.

| Event | Effect |
| --- | --- |
| Harness reassigns the provider's owner | Issuance returns `owner_mismatch`; `cloud status` warns. Re-run `amux direct cloud enable` to re-pin (voids earlier codes) |
| Provider recreated in Harness (new id) | `provider_mismatch` until you re-pin |
| Endpoint change | Restart `amux direct serve` (or re-run `amux direct enable --endpoint …`, which the daemon applies); bundles carry the running endpoints, and the provider re-registers with them within about a minute |
| Direct server not running (or `amux direct disable`) | The running provider withdraws the offer by re-registering within about a minute; until then requests answer `unavailable` |

## Administration boundary

**The policy operations are host-only.** `cloud_enable`, `cloud_disable` and
`cloud_status` are admin-socket operations that need the existing SubjectHost
proof, exactly like `enroll` and `revoke`.

**The provider can reach exactly two operations.** It already holds host
authority for the local daemon. It can send only `cloud_status` (at each
registration, and every 30 seconds while registered, to decide whether to
advertise) and `issue_cloud_code` (built
field by field from a typed frame), through `direct.RelayIssuer`.

**No other path changes the policy.** No relay frame can enable, disable, re-pin
or read the ceiling, and the direct HTTP server has no admin routes.

## Wire

### Relay: `direct-enroll-v1` (harnessproto)

**Advertisement.** The provider advertises the feature, with
`capabilities.direct = {serverId, endpoints, issuance:true}`, only when two
things hold at registration: it was started with `--direct-enroll`, and the
direct server reports issuance enabled. While registered it re-checks every
30 seconds. If the answer changes for two checks in a row, it registers again
(same token, panes carried over) so the advertisement never outlives the
server. Listing the feature in `--feature` never advertises it. The ceiling and pins stay on the host.

**Frames:**

- **`direct-enroll`** (orchestrator → provider), with `reqId` and
  `directEnroll`:
  `{issueKey, requester, codeChallenge, codeChallengeMethod:"S256", permissions, resource:{kind, workgroups?}, label?}`.
- **`direct-enroll-result`** (provider → orchestrator), with the same `reqId`
  and `directEnroll`:
  `{bundle, expiresAt, serverId}` on success, or `{error}` on refusal.

**Replies are bounded.**

- Each request gets exactly one reply within 15 seconds.
- At most 4 requests are in flight per connection. Past that the reply is
  `busy`.
- A request without a `reqId` is dropped.

**Error codes:**

| Code | Meaning |
| --- | --- |
| `cloud_disabled` | The feature isn't offered, or issuance is off |
| `owner_mismatch` | The requester isn't the pinned owner |
| `provider_mismatch` | This provider isn't the pinned provider |
| `invalid_request` | The request is malformed |
| `invalid_challenge` | The challenge isn't S256 over 32 bytes |
| `scope_exceeds_ceiling` | The permissions or resource are above the ceiling |
| `idem_conflict` | `issueKey` was reused for a different request |
| `issue_expired` | The code for that key was used or has expired. Start a new enrollment with a new key |
| `capacity_exhausted` | The outstanding-code cap is reached |
| `rate_limited` | The hourly cap is reached |
| `busy` | The provider already has 4 requests in flight |
| `unavailable` | The outcome is unknown. Retry with the **same** `issueKey` |

**Idempotency.** A retry with the same `issueKey` and the same request returns
the same bundle and never mints a second code. To make that possible, the code
itself is stored as replay material, mode 0600 in the host-only store. That
material is cleared once the code is consumed or expires. A tombstone then
refuses the key for 24 hours.

**Handling rules.** Neither frame may be queued durably, replayed, broadcast or
logged with its bundle.

### Token endpoint

The enrollment grant takes an optional `code_verifier` form field.

**Cloud codes** require the verifier:

| The verifier is… | Result | Is the code consumed? |
| --- | --- | --- |
| Missing or malformed (RFC 7636 §4.1: 43–128 characters of `[A-Za-z0-9-._~]`) | `invalid_request` | No |
| Wrong | `invalid_grant` | Yes |
| Correct, but the policy changed since issuance | `invalid_grant` | Yes |

**Manual codes** are unchanged. A stray `code_verifier` is `invalid_request`,
and the code is not consumed.

### Fixtures

The cloud fixtures are additive, in `docs/direct/fixtures/cloud-v1` with their
own `SHA256SUMS`. The S1 manifest at `docs/direct/fixtures/v1` is unchanged, and
a test pins its hash. The harnessproto golden files are
`harnessproto/testdata/direct-enroll-v1-*.json`.

## Storage

**The registry moves to schema v2.** `grants.json` v2 adds cloud codes, the
policy and the issuance records.

**Migration from v1 happens on open.** It is one atomic replace, and it keeps
every grant, code, revocation, used-refresh history and idempotency record. If
the write fails, the open fails and the v1 file is left untouched. It is never
reinitialized.

**Downgrade is refused.** The S1 reader (15fcf0e) refuses v2, so an older binary
can never redeem a cloud code as an unbound ordinary code. The
`internal/direct/s1compat` tests run that exact reader against v2 files.
