# Tailnet direct enrollment (cloud approval): the host side

The feature is `direct-approval-v1`. It replaces the broker relay
(`relayDirectEnroll`) for brokered enrollment. The full cross-repo contract
lives in Harness (`docs/direct-tailnet-enroll.md` there). This page covers what
amux implements and how to run it.

## How it works

1. **The offer.** The provider advertises the host's direct server in its
   registration. When the direct server is running with cloud enrollment
   enabled (`amux cloud enroll enable`), the provider sends:
   - `features` += `direct-approval-v1`;
   - `capabilities.direct.approval: true`;
   - `capabilities.direct.ceiling`: `{permissions, resource:"all_sessions"|"workgroups", workgroups?}`.
     It is the cloud policy's ceiling, and `all_sessions` never includes the
     amux console.

   The provider re-registers within about a minute when the approval flag or
   the ceiling changes.
2. **Harness issues an approval** (`hdap_…`, single-use, 5 minutes) to the
   authenticated owner. Harness never contacts the host.
3. **The client posts the approval directly to the host** over the tailnet:

   ```
   POST /direct/v1/enroll/cloud
   approval=hdap_…&code_challenge=<S256>&code_challenge_method=S256
   ```

   The body is form-encoded; a query string is refused. The client then
   redeems the code at `POST /direct/v1/token` with its `code_verifier`, as
   for any cloud code.
4. **The host verifies the approval** in this order:
   1. Cloud policy on.
   2. Replay: the same approval with the same challenge returns the same code
      without asking Harness again.
   3. Verify and consume the approval at Harness (below).
   4. Owner pin, then provider pin.
   5. Challenge, permissions and resource within the ceiling.
   6. Mint, idempotent by approval hash. The approval itself is never stored.

**QR v3 (another device).** A device-ticket approval is unbound.
Harness encodes it as
`amux-direct:3:` + base64url(`{"v":3,"server_id","endpoints","approval","expires_at"}`)
(`direct.EncodeApprovalBundle` / `DecodeApprovalBundle`). The scanning device
posts it to `/direct/v1/enroll/cloud` with its **own** challenge, and the
first device to do so binds it, at Harness and here.

## Responses

**200:** `{"code","expires_at","server_id"}`, with `Cache-Control: no-store`.

**Errors** are `{"error","reason"}`:

| HTTP | error | reason |
|---|---|---|
| 400 | `invalid_request` | `malformed` (bad approval or challenge, method not S256, or a query string) |
| 403 | `access_denied` | `ceiling` (permissions or scope above the host's ceiling) |
| 403 | `access_denied` | `ticket_expired`: the approval expired, or the cloud policy changed since the code was minted |
| 403 | `access_denied` | `ticket_used`: consumed with another challenge, or the code was already redeemed |
| 403 | `access_denied` | `owner_mismatch`: the requester is not the owner pin, or Harness answered `owner_changed` |
| 403 | `access_denied` | `provider_mismatch`, `approval_unknown`, `approval_mismatch` |
| 403 | `access_denied` | `cloud_disabled` |
| 429 | `temporarily_unavailable` | `rate_limited`, with `Retry-After` |
| 503 | `temporarily_unavailable` | `orchestrator_unreachable`: no provider is polling, or Harness did not answer in 10 s |
| 503 | `temporarily_unavailable` | `unavailable`: a transient failure. Retry with the same approval and challenge |

Refused requests count toward the same per-source limit as the token and
claim endpoints.

## The verify frame (harnessproto, existing provider connection)

This is the first provider-initiated request on the connection:

- **`HarnessMsg`** `direct-approval-verify` (`HDirectApprovalVerify`), with
  `reqId` and `directApproval: {approval, serverId, codeChallenge}`. There is
  no provider id in the body; Harness takes it from the authenticated
  connection.
- **`MuxMsg`** `direct-approval-result` (`MDirectApprovalResult`), with the
  same `reqId` and `directApprovalResult`:
  - ok: `{requester, permissions, resource{kind,workgroups?}, label, handoff?, expiresAt}`;
  - refused: `{error}` ∈ `approval_unknown`, `approval_expired`,
    `approval_used`, `approval_mismatch`, `owner_changed`, `unavailable`.

The provider and the direct server are separate processes, so the direct
server parks each verify in memory. The provider long-polls for verifies over
the host-only admin socket (`approval_lease`), sends the frame, and reports
Harness's answer back (`approval_settle`).

**Limits:**
- One reply within 10 s.
- At most 4 verifies in flight on each side; past that the answer is
  `unavailable`.
- With no provider poll in the last 30 s, the host answers
  `orchestrator_unreachable` at once.

Approvals are never written to disk, queued durably, replayed or logged. Logs
carry only the outcome and its timing.

## CORS (browser clients)

The server allows exact origins, never credentials, and allows
`Authorization` and `Content-Type` on preflight. The same gate covers
`/direct/v1/hello`, `/direct/v1/enroll/cloud`, `/direct/v1/token` and the
resource API.

With `amux device enable --tailnet`, two things make the Harness web origin
allowed:

- the provider records the registration ack's `webOrigin` (Harness's
  `HARNESS_PUBLIC_ORIGIN`) in its status file;
- the running server allows that origin live, re-read every few seconds, with
  no restart.

`--tailnet` also saves it as the default origin when one is known. Until
Harness sends `webOrigin`, the default falls back to deriving an origin from
`provider.toml`'s orchestrator address, and that address is the provider
endpoint, not the web app's origin. The fallback fix is:

```sh
amux device enable --tailnet --allow-origin https://harness.gcp.chynoweth.dev
```

`--no-default-origin` turns off both the default origin and the live
`webOrigin`.

## Operator checks

`amux device status` shows the latest enrollment exchange, including
`enroll_cloud` attempts with their reason. `amux device logs` shows a line per
exchange (`direct: enrollment …`), and the provider log shows
`direct-approval verify: <outcome> in <n>ms`. A request that never shows up
here did not reach the host: check the device's tailnet membership, ACLs and
DNS, and the probe the client reports.
