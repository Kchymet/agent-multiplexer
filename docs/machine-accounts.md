# Machine accounts v1

The daemon owns one central Claude profile and one central Codex profile per amux installation. The amux CLI/TUI (including the trusted console terminal CLI) configure login and select the configuration for new workloads. Both the TUI and web Harness show account status and subscription windows; web account access is read-only. Each view exposes at most one Claude and one Codex configuration. The implementation follows the [broker identity design](broker-identity-spec.md); this document describes the implemented subset and its limits.

## Operator setup

Open **Accounts** from the TUI rail (`s`). The initial screen shows Claude and Codex together, including reported usage, resets and freshness; Tab selects the harness for actions. `e` starts subscription login and `a` starts API-key login. Each existing central profile has one explicit `subscription` or `api` mode. In a host terminal, the equivalent commands are:

```sh
amux auth login claude
amux auth login codex
amux auth login claude --api-key
amux auth login codex --api-key
amux auth accounts --json
amux auth status codex
amux auth default claude:central
amux auth default codex:central
amux auth logout claude
```

Claude uses `claude setup-token`. The CLI runs that command and reads the resulting token with terminal echo disabled; `--token-stdin` also works. TUI completion is a protected, write-only token field: run `claude setup-token` in a trusted host terminal and paste its output there. Do not paste credentials into an agent conversation. There is no automated Claude browser issuance API in this adapter. Its token is opaque: account email, plan, expiry, and same-account identity cannot be inferred from it. It has no refresh token; renewal requires another setup-token ceremony.

Codex uses the installed native App Server's browser login. The browser must reach that machine's native callback, including when the UI is remote. Device-code login and a browser callback proxy are not implemented. Missing binaries or unsupported native operations produce explicit unavailable states. The native server must support externally supplied ChatGPT tokens for workload login; unsupported versions fail closed. The automated tests use fake native servers and private temporary credentials; they do not sign the operator out or perform a real provider login.

API-key login for both harnesses reads hidden terminal/TUI input, or a bounded
stdin line with `--api-key --token-stdin`. No option accepts a key value in argv.
The client requires advertised `supportedModes` containing `api` and a pending
ceremony echoing `mode: api`, `method: api-key` before accepting key input. An
older or ambiguous daemon fails closed with upgrade guidance. Keys travel only
on the existing authenticated, initiator-owned ceremony connection. The trusted
console exception is unchanged; web clients and ordinary agents cannot configure
accounts or submit secrets.

Claude subscription tokens and API keys replace the same private atomic record
(0700 directory, 0600 file, no-follow checks, monotonic generation). Its protected
launcher receives a typed credential and selects `CLAUDE_CODE_OAUTH_TOKEN` or
`ANTHROPIC_API_KEY` only at final exec, clearing competing inherited inputs. Codex
uses native `account/login/start` with `type: apiKey` over private stdio to its
single daemon-owned home. That transaction retains native writer shutdown and
rollback before reopening delivery. Workload App Servers receive the key through
private RPC with `cli_auth_credentials_store="ephemeral"`; they do not persist
an API key in session auth.json or a keyring. Default official provider endpoints
remain pinned; third-party endpoint configuration is outside this feature.

API identity and plan remain unknown/not applicable. API credentials never claim
a native account ID or ChatGPT refresh identity. The binding records an explicit
credential mode, and the daemon persists an opaque API generation independently
of native account IDs and process-local token generations. API-key replacement
and subscription/API transitions block existing bound workloads until authorized
explicit rebind. Default changes do not rebind workloads. A genuinely new binding
created before any credential exists can accept its first authorized login;
unknown legacy evidence remains blocked. API mode reports subscription usage as
`not-applicable`, clears subscription windows, and rejects Claude statusline quota
reports. API billing limits are not estimated from subscription quotas.

Protocol and native references: [App Server authentication](https://learn.chatgpt.com/docs/app-server)
and [ephemeral credential storage](https://learn.chatgpt.com/docs/config-file/config-reference).
The pinned Codex 0.157.1 synthetic-key checks exercise native login/account-read,
private central-to-workload delivery, first-login and replacement/rebind fencing,
and absence of a workload auth.json, without thread/turn/model requests.

Starting replacement login pauses **new credential delivery** for that central profile. Already running processes may retain delivered tokens. Cancel, timeout, disconnect, or failed acquisition restores a prior ready profile only after the native writer has stopped and its prior credential file has been restored. If a newer logout/login has advanced the epoch, the old attempt cannot restore anything. Cleanup failure leaves delivery blocked and exposes a fixed error. Logout fences delivery before native cleanup; `account_logout_cleanup_failed` (or `account_logout_cleanup_unavailable` when the native adapter is absent) remains visible after reconnect until successful recovery. Tokens already delivered cannot be recalled or revoked upstream by amux.

## Trusted console CLI

The existing amux console terminal may run the same `amux auth` commands. Its fixed, read-only session context selects a dedicated socket in its daemon-owned access directory. The socket authenticates the console's session credential with pinned TLS, resolves its live console role, rechecks generation/authority at effect admission, and binds login attempts to that connection. It accepts account queries only; ordinary agents, provider principals, generic actions and general inventory queries are denied. The host socket and regular-file session RPC do not gain account privileges. Reopen an existing console terminal after upgrading so its sandbox includes the exact socket grant.

Console Claude login **does not run native `claude setup-token`**, whose output contains the credential. Obtain the token in a trusted host terminal, then use the console terminal's hidden prompt or a protected stdin source. amux does not print the supplied token. Do not paste tokens into console agent chat or command arguments. Codex browser authorization URLs must be shown for interaction and may be captured by terminal/tool recording; the console is not a secret-free output channel. Account requests, completion tokens and ceremony responses themselves remain transient and never enter mailbox files, session actions/events, or durable account relay.

## Defaults and existing workloads

A fresh installation defaults to `claude:central` and `codex:central`. At first initialization, explicit legacy `claude.auth` / `codex.auth` settings seed the defaults. Thereafter, change defaults through Accounts or `amux auth default`; the persisted account service is authoritative. Existing host, Claude shared, and environment defaults/bindings remain internal compatibility state. They are not additional selectable account configurations. If a legacy default is active, Accounts says so; the central configuration becomes the new-workload default only after explicit local **Use for new workloads** or `amux auth default claude` / `amux auth default codex`. Viewing accounts never changes routing.

A workload persists its profile and source at creation. Changing a default never changes existing workload routing. Both the workgroup coordinator and initial worker must have a ready selected profile before creation. The service refuses corrupt account metadata instead of switching to a global source.

Codex pins the native account ID after first delivery. Reauthentication into the same native account preserves its thread; a different account is rejected for that bound workload. Claude pins a monotonically increasing credential version because an opaque replacement cannot prove same-account identity. Logout and relogin never reuse a Claude version.

An explicit local host operation stops a workload and rebinds it:

```sh
amux auth rebind claude:central WORKLOAD_ID
amux do start WORKLOAD_ID
```

Use `codex:central` similarly only when compatible with the persisted native thread account. A different or unknown target account for an existing account-bound Codex thread returns `account_rebind_requires_new_workload`; create a new workload instead. All remote account mutations, including rebind, are denied; use the amux CLI/TUI locally. Rebind is not automatic after login or default changes.

Metadata lives in the host-only `<amux data>/auth/accounts.json`; vendor secrets remain in their existing private stores. Atomic metadata transactions fence delivery and asynchronous callbacks by epoch and binding revision. Persisted `login-pending` profiles are retired on daemon startup; browser ceremonies are never reconstructed or replayed after a crash.

## Subscription limits

Codex windows come from native `account/rateLimits/read`. A missing field is unknown; an unsupported/unreachable endpoint is unavailable. A reported zero is distinct from a missing percentage.

Claude windows are observations from the documented statusline `rate_limits.five_hour` and `rate_limits.seven_day` fields. Only an authenticated, currently running, centrally bound workload with a matching delivered epoch/version can report them. The projection includes profile ID, delivery epoch, reporting session, runtime generation, and observation time. This is workload-reported evidence, not provider-authenticated account identity. Accounts without a supported statusline observation show unavailable/unknown rather than invented limits. amux does not estimate subscription capacity from transcript token counts.

Both UIs use `AccountLimitsTTLMillis = 300000`: observations older than five minutes are stale. `limits.observedAt` and `ceremony.expiresAt` are Unix **milliseconds**; window `resetsAt` is Unix **seconds**. Missing windows do not mean unlimited capacity. Reported quota never grants authority or determines an upstream account switch.

## Remote grants and transport

Account grants are independent of session/compute permissions:

```sh
amux provide install --account-status
# Explicitly deny account use while preserving other configured grants:
amux provide install --account-use=false
```

`--account-status` permits safe account/limits projections and defaults off. `--account-use` controls remote workload creation/start/prompt; it defaults on for writable session publication and can be explicitly disabled. These grants remain independent. They persist in `provider.toml`; corresponding environment variables are `AMUX_PROVIDER_ACCOUNT_STATUS` and `AMUX_PROVIDER_ACCOUNT_USE`.

`--account-manage=true` is retired and returns a message directing users to amux CLI/TUI. Old persisted or environment manage grants are ignored; reinstalling the provider clears the saved grant. Providers always advertise `capabilities.accounts.manage=false` and reject login, completion, polling, cancellation, default, logout and rebind requests, even from old clients with stale grants. The Harness server also rejects web account writes. The trusted amux console terminal can run `amux auth` through a dedicated local account socket; account status grants do not confer console access. Ordinary agents and provider principals cannot authenticate to this socket.

`accounts-v1` and its wire types remain compatible. Remote `account-request` / `account-result` frames support only `status`. Local daemon APIs retain safe default/logout/rebind and protected login/poll/cancel/complete/disconnect ceremonies. Local ceremonies never enter durable relay queues, replay, session actions, or event logs. Safe status rejects secret/attempt/initiator fields and never returns a ceremony URL.

## Canonical integration contract

Types are in [harnessproto/accounts.go](../harnessproto/accounts.go). `AccountSnapshot.ManagedOnly()` projects only `claude:central` and `codex:central`, retaining the actual `defaults` and CAS revisions even when a legacy source remains active. Internal readiness uses the full state, including coordinator and worker harnesses. The shared synthetic fixture is [accounts-v1.json](../harnessproto/testdata/accounts-v1.json). `Session.accountBinding = {profileId, source}` exposes only persisted routing metadata in authorized workload inventory; omission means unknown, never the current default. It carries no upstream identity or secrets.

Mutations use the target profile `revision`. Default selection additionally requires `defaultRevision` from `snapshot.defaultRevisions[profile.harness]`, so two clients choosing different profiles from one snapshot cannot both succeed. On conflict, refresh and ask the user to review the new state; do not silently retry.

TUI/CLI use `(*daemon.Client).Accounts(request, ceremony)` on a dedicated connection. Its `core.Action.Fields` envelope is exactly `{"request": JSON(AccountRequest)}`. Queries are `accounts` for status, `account-manage` for safe mutation, and `account-ceremony` for transient requests. Closing the client cancels its attempts. Ceremony states are `pending`, `succeeded`, `failed`, and `cancelled`, with a fifteen-minute deadline and terminal URLs cleared. Safe profile states are `unconfigured`, `login-pending`, `ready`, `blocked`, and `logged-out`.

V1 exposes one stable amux-owned central configuration per harness, preserving legacy sources internally; named multi-account profile management, remote rebind, device-code initiation, and a standalone Claude quota/identity API are not implemented. These limits do not defer login, protected Claude token completion, account/limits viewing, defaults, or stable workload bindings to design-only work.

### Claude launch diagnostics

A central Claude pane obtains its token after entering the sandbox, through its signed session mailbox. It does not connect to the host management socket. The mailbox request waits up to ten seconds and the launcher retries after five seconds. A running daemon or a copied Claude configuration does not by itself prove that this separate credential request was answered. On a new session, the child can run before reconciliation publishes the mailbox service record; this immediate failure also retries. It is not a connection timeout or evidence of a missing login.

Only a confirmed absent central credential recommends `amux auth login claude`. A mailbox timeout leaves credential status unknown; check daemon status from a trusted host terminal. Access denial, a blocked workload binding, conflicting settings, and an unreadable credential have separate guidance. Login does not restore revoked session authority or silently rebind an existing workload. Repeated failures in the same category print once; a changed category updates the guidance. Older daemons that return the ambiguous `central_credential_unavailable` code are treated as unknown, not as proof that login is missing.
