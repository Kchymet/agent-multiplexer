# Machine accounts v1

The daemon owns one central Claude profile and one central Codex profile per amux installation. The amux CLI/TUI (including the trusted console terminal CLI) configure login. Every workload follows its harness's central configuration; each new harness process uses the newest successfully configured login. Both the TUI and web Harness show account status and subscription windows; web account access is read-only. Each view exposes at most one Claude and one Codex configuration. The implementation follows the [broker identity design](broker-identity-spec.md); this document describes the implemented subset and its limits.

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
launcher negotiates typed delivery (old launchers cannot receive API keys) and selects `CLAUDE_CODE_OAUTH_TOKEN` or
`ANTHROPIC_API_KEY` only at final exec, clearing competing inherited inputs. Codex
uses native `account/login/start` with `type: apiKey` over private stdio to its
single daemon-owned home. That transaction retains native writer shutdown and
rollback before reopening delivery. Workload App Servers receive the key through
private RPC with `cli_auth_credentials_store="ephemeral"`; they do not persist
an API key in session auth.json or a keyring. Default official provider endpoints
remain pinned; third-party endpoint configuration is outside this feature.

API identity and plan remain unknown/not applicable. API credentials never claim
a native account ID or ChatGPT refresh identity. The daemon persists an opaque
API generation independently of native account IDs and process-local token
generations. A replacement API key, a different subscription account, or a
subscription/API transition never retargets a running process; the next process
launch of every workload receives it (see
[Existing workloads](#defaults-and-existing-workloads)).
API mode reports subscription usage as
`not-applicable`, clears subscription windows, and rejects Claude statusline quota
reports. API billing limits are not estimated from subscription quotas.

Protocol and native references: [App Server authentication](https://learn.chatgpt.com/docs/app-server)
and [ephemeral credential storage](https://learn.chatgpt.com/docs/config-file/config-reference).
The pinned Codex 0.157.1 synthetic-key checks exercise native login/account-read,
private central-to-workload delivery, running-process fencing and next-launch
replacement, and absence of a workload auth.json, without thread/turn/model requests.

Starting replacement login pauses **new credential delivery** for that central profile. Already running processes may retain delivered tokens. Cancel, timeout, disconnect, or failed acquisition restores a prior ready profile only after the native writer has stopped and its prior credential file has been restored. If a newer logout/login has advanced the epoch, the old attempt cannot restore anything. Cleanup failure leaves delivery blocked and exposes a fixed error. Logout fences delivery before native cleanup; `account_logout_cleanup_failed` (or `account_logout_cleanup_unavailable` when the native adapter is absent) remains visible after reconnect until successful recovery. Tokens already delivered cannot be recalled or revoked upstream by amux.

## Trusted console CLI

The existing amux console terminal may run the same `amux auth` commands. Its fixed, read-only session context selects a dedicated socket in its daemon-owned access directory. The socket authenticates the console's session credential with pinned TLS, resolves its live console role, rechecks generation/authority at effect admission, and binds login attempts to that connection. It accepts account queries only; ordinary agents, provider principals, generic actions and general inventory queries are denied. The host socket and regular-file session RPC do not gain account privileges. Reopen an existing console terminal after upgrading so its sandbox includes the exact socket grant.

Console Claude login **does not run native `claude setup-token`**, whose output contains the credential. Obtain the token in a trusted host terminal, then use the console terminal's hidden prompt or a protected stdin source. amux does not print the supplied token. Do not paste tokens into console agent chat or command arguments. Codex browser authorization URLs must be shown for interaction and may be captured by terminal/tool recording; the console is not a secret-free output channel. Account requests, completion tokens and ceremony responses themselves remain transient and never enter mailbox files, session actions/events, or durable account relay.

## Defaults and existing workloads

Every workload — new, existing, dormant, archived, and the console — follows its
harness's central configuration (`claude:central`, `codex:central`). The default
is always central; host, shared and environment sources are retired and cannot
be selected (`account_legacy_source_retired`). Viewing accounts never changes routing.

**Next process launch.** The newest successfully configured central login —
including a different account, a subscription↔API change, or a replacement API
key — applies when a workload's harness process is next launched or restarted.
Login never stops a workload. Each launch is fenced by a daemon-allocated launch
(a sequence plus a random nonce; metadata keeps only its hash):

- Codex: the supervisor allocates a launch when it starts an App Server process
  (a reconnect to a live process keeps its launch). The launch never enters the
  sandbox.
- Claude: the daemon allocates a launch for each resolved agent pane and passes
  it to the in-sandbox launcher's argv, which its exec into Claude replaces. A
  resolve whose pane the engine reuses (a reattach) or fails to start is retired,
  so every started process holds a fresh launch.

The first delivery to a launch pins the current mode, account and credential
version, and retires every older launch. Further deliveries to the same launch
(a timeout or lost-response retry, an abandoned duplicate, a reconnect,
same-account Codex refresh) receive the same snapshot. After a replacement they
are refused with `account_runtime_superseded` — a running process never adopts
another login; restart the workload to use it. An older, unallocated, or
nonce-mismatched launch is refused. Logout, pending-login, cancellation and
binding-revision fences apply as before. Each binding keeps the evidence of the
current runtime and of up to eight earlier ones. This is a runtime-consistency
guarantee, not isolation from a malicious process inside the same session (all
tabs of a session share its principal); a Codex launch never enters the sandbox.

**Codex threads.** A new Codex process resumes the exact persisted thread under
the current central account; the account the thread previously ran under is
kept in its identity as history (`priorChatgptAccounts`) and a notice records
the change. A resume target is never replaced by an empty thread: a resume
error fails the launch visibly and leaves the identity unchanged. The pinned
Codex 0.157.1 accepts another account's injected tokens for a resumed thread and
replays its full history, including opaque encrypted reasoning
([evidence](evidence/auth-follow-central/CHECKPOINT.md)). The real backend may
refuse another account's encrypted reasoning; such a turn fails visibly and the
thread and its history are kept.

**Migration.** The account store's version 2 migration runs once,
automatically, under the store lock: every default and binding moves to
central, the original metadata is kept once as `accounts.v1.json`, and newer,
unknown or malformed metadata is refused without being rewritten. No credential
is copied: host and shared logins hold rotating refresh tokens (a copy would
fork the refresh chain) and environment credentials are not amux's to move. A
harness that had no central login is reported as *migrated — central login
required* in Accounts and `amux auth status` until its first central login;
launches meanwhile use the normal central login flow. Saved `claude.auth` /
`codex.auth` legacy values only record this evidence; `amux config set` accepts
only `central`. Central Codex auth requires App Server control, so
`codex.control pty` is reported and overridden. A Codex session last run under
PTY control has its rollout imported once into its structured event log (before
any notices already there), so its earlier history stays readable. A log that
already holds structured events is kept as is. The rewrite never runs while that
session's runtime is running or starting. The outcome is recorded beside the log
and inside it; until then a failed import refuses only that session's launch and
is retried. The import runs once per session, inside that launch: a launch
admitted under the daemon-wide gate holds it while a large rollout is mapped
(each line is bounded at 16 MiB; the rollout is read once).

`amux auth rebind <claude|codex> WORKLOAD_ID` is optional: it stops one workload
so its next start uses the current login now rather than at its next launch.
All remote account mutations, including rebind, are denied; use the amux CLI/TUI locally.

Metadata lives in the host-only `<amux data>/auth/accounts.json`; vendor secrets remain in their existing private stores. Atomic metadata transactions fence delivery and asynchronous callbacks by epoch, binding revision and launch. Persisted `login-pending` profiles are retired on daemon startup; browser ceremonies are never reconstructed or replayed after a crash. A corrupt central Claude record never blocks the repairing login (delivery refuses it until then); an insecure credential store fails closed until its permissions are fixed.

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

`accounts-v1` and its wire types remain compatible. Remote `account-request` / `account-result` frames support only `status`. Local daemon APIs retain safe default (central only)/logout/rebind and protected login/poll/cancel/complete/disconnect ceremonies. Local ceremonies never enter durable relay queues, replay, session actions, or event logs. Safe status rejects secret/attempt/initiator fields and never returns a ceremony URL.

## Canonical integration contract

Types are in [harnessproto/accounts.go](../harnessproto/accounts.go). `AccountSnapshot.ManagedOnly()` projects only `claude:central` and `codex:central`, retaining the actual `defaults` and CAS revisions even when a legacy source remains active. Internal readiness uses the full state, including coordinator and worker harnesses. The shared synthetic fixture is [accounts-v1.json](../harnessproto/testdata/accounts-v1.json). `Session.accountBinding = {profileId, source}` exposes only persisted routing metadata in authorized workload inventory; omission means unknown, never the current default. It carries no upstream identity or secrets.

Mutations use the target profile `revision`. Default selection additionally requires `defaultRevision` from `snapshot.defaultRevisions[profile.harness]`, so two clients choosing different profiles from one snapshot cannot both succeed. On conflict, refresh and ask the user to review the new state; do not silently retry.

TUI/CLI use `(*daemon.Client).Accounts(request, ceremony)` on a dedicated connection. Its `core.Action.Fields` envelope is exactly `{"request": JSON(AccountRequest)}`. Queries are `accounts` for status, `account-manage` for safe mutation, and `account-ceremony` for transient requests. Closing the client cancels its attempts. Ceremony states are `pending`, `succeeded`, `failed`, and `cancelled`, with a fifteen-minute deadline and terminal URLs cleared. Safe profile states are `unconfigured`, `login-pending`, `ready`, `blocked`, and `logged-out`.

V1 exposes one stable amux-owned central configuration per harness that every workload follows; named multi-account profile management, remote rebind, device-code initiation, and a standalone Claude quota/identity API are not implemented. These limits do not defer login, protected Claude token completion, account/limits viewing, defaults, or stable workload bindings to design-only work.

### Claude launch diagnostics

A central Claude pane obtains its token after entering the sandbox, through its signed session mailbox. It does not connect to the host management socket. The mailbox request waits up to ten seconds and the launcher retries after five seconds. A running daemon or a copied Claude configuration does not by itself prove that this separate credential request was answered. On a new session, the child can run before reconciliation publishes the mailbox service record; this immediate failure also retries. It is not a connection timeout or evidence of a missing login.

Only a confirmed absent central credential recommends `amux auth login claude`. A mailbox timeout leaves credential status unknown; check daemon status from a trusted host terminal. Access denial, paused delivery (login pending, logged out, blocked), a superseded process (the login changed after launch: restart the workload), conflicting settings, and an unreadable credential have separate guidance. Login does not restore revoked session authority and never switches a running process; a launcher started without a daemon-allocated launch exits immediately. Repeated failures in the same category print once; a changed category updates the guidance. Older daemons that return the ambiguous `central_credential_unavailable` code are treated as unknown, not as proof that login is missing.
