# Machine accounts v1

The daemon owns one central Claude profile and one central Codex profile per amux installation. Accounts in the TUI and web Harness configure login, show identity and subscription windows, and select defaults for new workloads. The implementation follows the [broker identity design](broker-identity-spec.md); this document describes the implemented subset and its limits.

## Operator setup

Open **Accounts** from the TUI settings menu. In a host terminal, the equivalent commands are:

```sh
amux auth login claude
amux auth login codex
amux auth accounts --json
amux auth status codex
amux auth default claude:central
amux auth default codex:central
amux auth logout claude
```

Claude uses `claude setup-token`. The CLI runs that command and reads the resulting token with terminal echo disabled; `--token-stdin` also works. TUI/web completion is a protected, write-only token field: run `claude setup-token` in a trusted host terminal and paste its output there. Do not paste credentials into an agent conversation. There is no automated Claude browser issuance API in this adapter. Its token is opaque: account email, plan, expiry, and same-account identity cannot be inferred from it. It has no refresh token; renewal requires another setup-token ceremony.

Codex uses the installed native App Server's browser login. The browser must reach that machine's native callback, including when the UI is remote. Device-code login and a browser callback proxy are not implemented. Missing binaries or unsupported native operations produce explicit unavailable states. The native server must support externally supplied ChatGPT tokens for workload login; unsupported versions fail closed. The automated tests use fake native servers and private temporary credentials; they do not sign the operator out or perform a real provider login.

Starting replacement login pauses **new credential delivery** for that central profile. Already running processes may retain delivered tokens. Cancel, timeout, disconnect, or failed acquisition restores a prior ready profile only after the native writer has stopped and its prior credential file has been restored. If a newer logout/login has advanced the epoch, the old attempt cannot restore anything. Cleanup failure leaves delivery blocked and exposes a fixed error. Logout fences delivery before native cleanup; `account_logout_cleanup_failed` (or `account_logout_cleanup_unavailable` when the native adapter is absent) remains visible after reconnect until successful recovery. Tokens already delivered cannot be recalled or revoked upstream by amux.

## Defaults and existing workloads

A fresh installation defaults to `claude:central` and `codex:central`. At first initialization, explicit legacy `claude.auth` / `codex.auth` settings seed the defaults. Thereafter, change defaults through Accounts or `amux auth default`; the persisted account service is authoritative. Host, Claude shared, and environment modes remain selectable legacy profiles.

A workload persists its profile and source at creation. Changing a default never changes existing workload routing. Both the workgroup coordinator and initial worker must have a ready selected profile before creation. The service refuses corrupt account metadata instead of switching to a global source.

Codex pins the native account ID after first delivery. Reauthentication into the same native account preserves its thread; a different account is rejected for that bound workload. Claude pins a monotonically increasing credential version because an opaque replacement cannot prove same-account identity. Logout and relogin never reuse a Claude version.

An explicit local host operation stops a workload and rebinds it:

```sh
amux auth rebind claude:central WORKLOAD_ID
amux do start WORKLOAD_ID
```

Use `codex:central` similarly only when compatible with the persisted native thread account. A different or unknown target account for an existing account-bound Codex thread returns `account_rebind_requires_new_workload`; create a new workload instead. Remote rebind is deliberately denied in v1: account-manage alone is not authority over an arbitrary workload. Rebind is not automatic after login or default changes.

Metadata lives in the host-only `<amux data>/auth/accounts.json`; vendor secrets remain in their existing private stores. Atomic metadata transactions fence delivery and asynchronous callbacks by epoch and binding revision. Persisted `login-pending` profiles are retired on daemon startup; browser ceremonies are never reconstructed or replayed after a crash.

## Subscription limits

Codex windows come from native `account/rateLimits/read`. A missing field is unknown; an unsupported/unreachable endpoint is unavailable. A reported zero is distinct from a missing percentage.

Claude windows are observations from the documented statusline `rate_limits.five_hour` and `rate_limits.seven_day` fields. Only an authenticated, currently running, centrally bound workload with a matching delivered epoch/version can report them. The projection includes profile ID, delivery epoch, reporting session, runtime generation, and observation time. This is workload-reported evidence, not provider-authenticated account identity. Accounts without a supported statusline observation show unavailable/unknown rather than invented limits. amux does not estimate subscription capacity from transcript token counts.

Both UIs use `AccountLimitsTTLMillis = 300000`: observations older than five minutes are stale. `limits.observedAt` and `ceremony.expiresAt` are Unix **milliseconds**; window `resetsAt` is Unix **seconds**. Missing windows do not mean unlimited capacity. Reported quota never grants authority or determines an upstream account switch.

## Remote grants and transport

Account grants are independent of session/compute permissions:

```sh
amux provide install --account-status --account-manage
# Explicitly deny account use while preserving other configured grants:
amux provide install --account-use=false
```

`--account-status` permits safe account/limits projections. `--account-manage` permits default/logout and transient login ceremonies, but does not imply status or session access. Both default off. `--account-use` controls remote workload creation/start/prompt; it defaults on for writable session publication and can be explicitly disabled. These flags persist in `provider.toml`; corresponding environment variables are `AMUX_PROVIDER_ACCOUNT_STATUS`, `AMUX_PROVIDER_ACCOUNT_MANAGE`, and `AMUX_PROVIDER_ACCOUNT_USE`.

`accounts-v1` advertises explicit `capabilities.accounts = {status, manage, use}`. Safe `account-request` / `account-result` frames support status/default/logout. Local host APIs additionally support rebind. Separate transient `account-ceremony` request/result frames support login/poll/cancel/complete/disconnect. They never enter durable relay queues, replay, session actions, or event logs. The web Harness must use its authenticated transient TLS route; the old durable database relay cannot carry ceremonies. Its implementation is maintained in the Harness repository.

The relay supplies an authenticated browser-connection `initiator`; the daemon additionally prefixes its host connection identity. Poll/cancel/complete require both that owner and the opaque attempt ID. Disconnect retires all attempts for the initiator, including a start whose response was lost. Native errors are replaced with fixed service codes. Safe status always rejects secret/attempt/initiator fields and never returns a ceremony URL.

## Canonical integration contract

Types are in [harnessproto/accounts.go](../harnessproto/accounts.go). The shared synthetic fixture is [accounts-v1.json](../harnessproto/testdata/accounts-v1.json). `Session.accountBinding = {profileId, source}` exposes only persisted routing metadata in authorized workload inventory; omission means unknown, never the current default. It carries no upstream identity or secrets.

Mutations use the target profile `revision`. Default selection additionally requires `defaultRevision` from `snapshot.defaultRevisions[profile.harness]`, so two clients choosing different profiles from one snapshot cannot both succeed. On conflict, refresh and ask the user to review the new state; do not silently retry.

TUI/CLI use `(*daemon.Client).Accounts(request, ceremony)` on a dedicated connection. Its `core.Action.Fields` envelope is exactly `{"request": JSON(AccountRequest)}`. Queries are `accounts` for status, `account-manage` for safe mutation, and `account-ceremony` for transient requests. Closing the client cancels its attempts. Ceremony states are `pending`, `succeeded`, `failed`, and `cancelled`, with a fifteen-minute deadline and terminal URLs cleared. Safe profile states are `unconfigured`, `login-pending`, `ready`, `blocked`, and `logged-out`.

V1 has one stable central profile per harness plus legacy sources; named multi-account profile management, remote rebind, device-code initiation, and a standalone Claude quota/identity API are not implemented. These limits do not defer login, protected Claude token completion, account/limits viewing, defaults, or stable workload bindings to design-only work.
