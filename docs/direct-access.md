# Direct access (S1)

`amux direct` lets the Harness web client and the Android app reach this host's
sessions over a route you already have (your LAN, or a VPN/tailnet you already
run), with no cloud service in the data path. It is opt-in and off by default.

This is the first delivery step. Cloud-assisted enrollment, Headscale and a
Harness identity provider are later work and nothing here depends on them.

## What it is

`amux direct serve` is a separate process: an HTTPS listener on a private
address. It is the **remote authorization boundary**. Every request passes a
destination-issued grant check before the process makes any elevated call to
the local daemon (through `internal/hostbackend`, as the Harness relay provider
does). The local daemon's authentication and the Harness relay wire are
unchanged.

S1 serves exactly:

| Route | Permission | What it does |
| --- | --- | --- |
| `POST /direct/v1/hello` | none | Server identity proof bound to the client's nonce, endpoint and protocol |
| `POST /direct/v1/token` | none (code / refresh token) | Enrollment and refresh |
| `POST /direct/v1/revoke` | none (the token) | Client sign-out (revokes the whole grant) |
| `GET /direct/v1/sessions` | read | The grant's inventory |
| `GET /direct/v1/sessions/watch` (WebSocket) | read | The same inventory, pushed on change |
| `GET /direct/v1/sessions/{sid}/history?before_seq=&limit=` | read | One newest-first transcript page |
| `GET /direct/v1/sessions/{sid}/stream?after=` | read | Live transcript tail (SSE format) |
| `POST /direct/v1/sessions/{sid}/actions` | steer | `prompt` or `stop` |

Everything else fails closed:

- interject, permission answers, goals and lifecycle verbs;
- accounts and panes;
- raw runtime records and host paths.

Inventory rows are an allowlisted projection. Working directories, process ids,
account bindings and local attach affordances are never sent.

**The console is never exposed.** The machine-wide amux console carries operator
and account powers. No grant can reach it, `all_sessions` included:
`all_sessions` means every ordinary session, and `--workgroup console` is
refused. An explicit console grant would be a separately reviewed feature.

## Operator guide

1. **Get a certificate.** Browsers need one they trust. A good choice is a
   certificate from a public CA for a name you own that resolves to the host's
   private address (DNS-01 validation needs no inbound exposure). Android can
   also use a private CA it trusts. No hosted certificate service is involved.
2. **Run the server.**

   ```sh
   amux direct serve --listen 192.168.1.20:8443 \
     --endpoint https://amux.home.example:8443 \
     --allow-origin https://harness.example \
     --tls-cert ~/certs/amux.pem --tls-key ~/certs/amux.key
   ```

   - **Bind address.** `--listen` must be a private or loopback IP literal:
     RFC 1918, 100.64/10, ULA, link-local or loopback. Anything else, including
     `0.0.0.0`, needs `--allow-public-bind`.
   - **HTTPS.** It is mandatory, even on a private network.
   - **`--endpoint`.** Lists the exact origins clients dial. A request whose
     `Host` is not one of them gets `421 endpoint_not_bound`.
   - **`--allow-origin`.** Lists the exact browser origins allowed. There is
     no wildcard and no credentialed CORS.
3. **Enroll a client.**

   ```sh
   amux direct enroll --label "Pixel" --permissions read,steer --workgroup 771951
   ```

   Choose the scope explicitly: `--all-sessions`, or one or more
   `--workgroup <root-id>`. Permissions are `read` or `read,steer`. The command
   prints one line, `amux-direct:1:<base64url JSON>`. Paste it into the client
   within its lifetime (default 10 minutes, at most 1 hour). It works once.
4. **Manage clients.**
   - `amux direct clients list` shows the enrolled clients.
   - `amux direct clients revoke <client-id>` revokes one. It returns only
     after that client's open streams have closed and its in-flight calls have
     finished or been cancelled.
   - `amux direct reset-identity --yes` replaces the server identity and
     revokes every client.

### Administration is host-only

The admin socket is `admin.sock` in the direct state directory. The CLI and the
running server talk over it.

**Location.** The directory is `~/.local/state/amux/direct/v1`. It is resolved
from the login account, not `$HOME`, with mode 0700 and files 0600. Pane
sandboxes never mount it (`TestManifestNeverMountsDirectState`).

**Reaching the socket is not authority.** Before any request is read, the
caller must prove the current host credential:

- It loads the credential from the protected, HOME-independent directory.
- It signs the daemon's own issuer-signed challenge.
- The direct server relays the challenge and the proof to the daemon's host
  socket (`daemon.RelayHostProof`).

The daemon admits only `SubjectHost` keys there. Session, provider and
direct-client credentials are refused, and a refused caller writes nothing.

**When no server is running,** `list`, `revoke` and `reset-identity` take the
store's exclusive lock and require the same host proof. They never create a
store. `enroll` needs a running server, because it publishes the server's
endpoints into the bundle. All of `amux direct` refuses to run inside an agent
session.

## Wire contract

- **Fixtures.** The fixtures in [`direct/fixtures/v1`](direct/fixtures/v1) are
  the contract that clients copy byte for byte. Pin [`SHA256SUMS`](direct/fixtures/v1/SHA256SUMS).
- **Go reference.** The types are in `internal/direct/wire.go`. The reference
  client checks are `VerifyHello` and `DecodeBundle`.
- **Changing a fixture** is a cross-repo contract change.

### Identity and hello

The server has a dedicated P-256 key. It is separate from the daemon's keys
and persists across restarts. `server_id` is the key's RFC 7638 SHA-256 JWK
thumbprint (base64url), and it is carried in the bundle.

**Request.** `POST /direct/v1/hello` with this body:

```json
{"protocol":"amux-direct/1","client_nonce":"<base64url 32 bytes>","endpoint":"<the origin dialed>"}
```

**Response.** `{"proof": "<compact JWS>"}`.

- Header: `alg` ES256, `typ` `amux-direct-hello+jws`, and the public `jwk`.
- Payload: `server_id`, `protocol`, `endpoint`, `client_nonce`, `iat`, and
  `exp` (`iat` + 60).

**Client checks.** Before sending any code or credential, the client verifies,
with a maintained JOSE library:

- the algorithm, which must be ES256 only;
- that the key is a public P-256 JWK whose thumbprint equals the expected
  `server_id`;
- the signature;
- that the nonce, endpoint and protocol match what it sent;
- `iat` and `exp`, within ±60 s of skew.

Hello does not replace TLS validation. A client validates TLS first, and an
Android pin must check the configured destination, never trust-all.

### Tokens (RFC 6749, 6750, 7009)

**Enrollment.** `POST /direct/v1/token`, form-encoded:

```
grant_type=urn:amux:params:oauth:grant-type:enrollment-code&code=…&client_label=…&client_kind=web|android
```

**Refresh.** `grant_type=refresh_token&refresh_token=…`.

**Token response.** Sent with `no-store`:

```json
{"access_token":"amxa_…","token_type":"Bearer","expires_in":600,"refresh_token":"amxr_…",
 "scope":"read steer","client_id":"c_…","server_id":"…","grant_expires_at":…,
 "resource_scope":{"kind":"workgroups","workgroups":["771951"]}}
```

**Errors.** `{"error":"invalid_request"|"invalid_grant"|"unsupported_grant_type"|"temporarily_unavailable"}`.

**Rules:**

- **Secrets.** Never put a secret in a URL. A query string on the token
  endpoint is rejected.
- **Token format.** Every token carries 32 random bytes. The server stores
  only SHA-256 hashes.
- **Lifetimes.** Access tokens last 10 minutes at most and live in server
  memory only, so a restart invalidates them. Refresh lasts 30 days idle and
  90 days absolute. All lifetimes are configurable downward only.
- **Strict rotation.** Each refresh consumes the presented token and durably
  commits its successor before responding. A consumed refresh token presented
  again revokes the whole grant. There is no grace window.
- **Rotation limit.** A grant's consumed-token history is never truncated. At
  20,000 rotations the grant ends and the client must enroll again.
- **Clients serialize refresh per backend, across browser tabs too.** They
  never automatically retry an ambiguous refresh POST with the old token. A
  response lost after the commit means enrolling again; report that as
  "reauthorization required".
- **Rate limits are per source.** Failed attempts are counted per source
  address: 5 per minute, then `429` for that source only. One peer presenting
  bad tokens cannot block other clients' renewals.
- **Revoke.** `POST /direct/v1/revoke` with `token=…` revokes the grant that
  owns the token. It always answers 200.

These are bearer credentials. A stolen live token works until it expires or is
revoked, and script running on the web origin can reach stored tokens. Nothing
here claims key possession or protection from a compromised frontend.

### Resource API

**Authentication.** Requests use `Authorization: Bearer amxa_…`. Clients send
`credentials: 'omit'` and refuse redirects. The server ignores and never sets
cookies.

**Errors.** The body is `{"code","error"}`.

| Status | Code | Client action |
| --- | --- | --- |
| 401 | `auth.invalid` | Unknown token (e.g. after a server restart): one serialized refresh, then retry once |
| 401 | `auth.expired` | Refresh, then retry |
| 401 | `auth.revoked` | The grant is gone: purge this backend only, and enroll again |
| 403 | `denied` | Missing `steer` |
| 403 | `origin_not_allowed` | — |
| 404 | `not_found` | Unknown, out-of-scope and console sessions all look the same |
| 409 | `archived` | Target or one of its ancestors is archived |
| 409 | `unsupported_verb` | — |
| 409 | `idem_in_progress` | — |
| 409 | `idem_ambiguous` | — |
| 421 | `endpoint_not_bound` | — |
| 422 | `rejected` | The daemon refused the action |
| 422 | `idem_conflict` | — |
| 429 | `rate_limited` | — |
| 503 | `unavailable` | — |

**Order of checks.** Credential, then permission, then (for actions) verb and
field validation. A failure at any of these reaches no backend at all. Only
then does the server read the fresh inventory (`Snapshot`) to check that the
target is in scope and not archived. An out-of-scope target causes no `Apply`,
transcript read or stream.

**Scope.** It is judged from current daemon state on every request: the
workgroup root plus every row whose root chain leads to it. Nothing
client-supplied counts.

**Actions.** `POST /direct/v1/sessions/{sid}/actions` with
`{"verb":"prompt","fields":{"text":"…"},"idem_key":"…"}` or
`{"verb":"stop","fields":{},"idem_key":"…"}`. Fields are an exact allowlist.

**Idempotency.** It is grant-scoped and durable, and the record matches the
body.

- **Before execution.** The key is recorded before the action runs. Expiry
  before execution denies the action.
- **During execution.** Once handed to the daemon, the call runs under a
  60-second execution bound. Token expiry and client disconnect do not cancel
  it; revocation does.
- **Outcome known.** A daemon refusal, or a failure before the daemon was
  reached, is recorded as a known outcome and replayed for the same key.
- **Outcome unknown.** Cancellation, or a transport failure after dispatch,
  is recorded as ambiguous. That key never executes again, and its records are
  never pruned.
- **Capacity.** A grant can hold 1,024 records at most. Unresolved ones count
  toward that, so at the limit new actions fail closed with `429`.
- **Crashes.** Exactly-once execution across a process crash is not claimed.

**Transcripts.** They use amux's own sequence space and never reuse a relay
cursor. The stream ends with a terminal SSE event:

- `event: auth` with `{"code":"auth.expired"}` or `{"code":"auth.revoked"}`;
- `event: end` with `{"code":"not_found"|"unavailable"}`.

The stream closes at access-token expiry even when idle. A stalled daemon call
is cancelled then. Reconnect with `?after=<last seq>` after refreshing.

**Watch (WebSocket).**

- **Authentication.** The first frame must be `{"type":"auth","access_token":"…"}`,
  within 5 seconds and at most 4 KiB. Nothing is sent before it is verified.
- **Messages.** Then `welcome`, then `snapshot` whenever the filtered
  inventory changes.
- **Close codes:**

  | Code | Reason |
  | --- | --- |
  | 4001 | `auth.invalid` |
  | 4002 | `auth.expired` |
  | 4401 | `auth.revoked` |
  | 4403 | `denied` |

  [`ws-close-codes.json`](direct/fixtures/v1/ws-close-codes.json) is the table.

### Revocation

**What a revoke does.** Revoking (host CLI, client sign-out, or refresh reuse)
does all of the following under the authority's single lock, then waits:

- marks the grant revoked;
- invalidates its access and refresh tokens;
- prevents any new admission;
- cancels the grant's in-flight leases.

The acknowledgment comes only after every registered stream and call of that
grant has finished. A side effect already admitted may have completed; nothing
is rolled back. Other clients are unaffected.

**If the revocation cannot be written to disk,** it still takes effect in the
running process, but the command reports failure, not success. Repair the
storage and run the revoke again **before restarting**: a restart re-reads
disk and would revive the unpersisted grant.

## S1 limits

- **Storage.** The registry is one JSON file, rewritten in full on every
  change. Lookups scan linearly. This is sized for a handful of personal
  devices, not many clients.
- **Ambiguous writes.** A durable write whose directory sync fails after the
  rename is "ambiguous". The process keeps the new state, issues no credential
  or code that depends on it, and refuses further issuance until it is
  restarted and re-reads disk.
- **Browsers.**
  - **Chromium.** Calls from a public page to a private address need the
    Local Network Access permission, and a denial is visible only as a
    network failure. Clients must not report a timeout or fetch `TypeError` as
    "Headscale enrollment missing".
  - **Other browsers and tailnets.** End-to-end behaviour must be verified
    before support is claimed.
