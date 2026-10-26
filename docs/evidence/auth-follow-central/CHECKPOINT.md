# Auth follows central — design and evidence (2026-10-25)

Branch `amux/355340-c0523b`. User-facing behaviour:
[machine accounts](../../machine-accounts.md#defaults-and-existing-workloads).
Formal properties: P11 and P17 in the [broker identity spec](../../broker-identity-spec.md).

## Native evidence (Codex 0.157.1, CI-pinned, sha256 verified)

`internal/codexapp/cross_account_resume_smoke_test.go` is opt-in (`AMUX_TEST_CODEX_BIN`). It uses
synthetic unsigned JWTs and a local TLS fake ChatGPT backend. It makes no real account or network
calls and copies no credential. Request compression is disabled so the fake can read bodies.

1. Phase 1 runs as `acct-alpha`. The fake returns a reasoning item with `encrypted_content`.
2. Phase 2 is a new App Server running `thread/resume` on the same thread with `acct-bravo` tokens
   injected. The thread id is unchanged, the request is sent with the `acct-bravo` header and no
   `previous_response_id`, and its `input` contains, in order, `user:PROMPT-ONE`,
   `reasoning:ENC-acct-alpha-1`, `assistant:answer 1`, `user:PROMPT-TWO`.
3. Phase 3 runs as `acct-charlie`. The backend rejects the turn with `invalid_encrypted_content`.
   The turn fails, the identity keeps the same thread, and `thread/start` is never called.
4. Phase 4 recovers on the same thread with the full ordered history.
5. Within one process (reconnect), the bound account is still enforced.

Conclusion: continuing a thread under another account is supported natively. The earlier refusal
was amux policy. Residual risk: the real backend may refuse another account's encrypted reasoning,
which a fake cannot prove either way. If it does, that turn fails visibly and the thread is kept.

## Design

- **Store v2 migration.** It runs once, idempotently, under the store lock. Every default and
  binding (live, dormant, archived, console) moves to `<h>:central`. The v1 file is kept once
  as `accounts.v1.json`. Newer, unknown or malformed metadata is refused and never rewritten.
  No credential is copied: rotating refresh chains must keep one owner. A harness without a
  central login reports *migration pending — central login required*
  (`AccountSnapshot.migrationPending`). Legacy sources cannot be selected again.
- **Launch fence.** The daemon allocates a launch (a sequence plus a 24-byte random nonce; only
  the hash is persisted) for each Claude agent-pane resolve and each Codex App Server process.
  - The first delivery pins mode, account and version, and retires every older pending launch.
  - Repeated deliveries to the same launch (timeout or lost-response retry, abandoned duplicate,
    reconnect, same-account refresh) return the same snapshot. After a replacement they get
    `account_runtime_superseded`.
  - An older, unallocated or nonce-mismatched launch is refused.
  - `ensurePane` retires a launch whose resolve started no process (the engine reused the
    running pane, or the spawn failed). A pane that exits between classification and `Ensure`
    is respawned with its own fresh launch.
  - Epoch, pending-login and revision fences are unchanged. Up to eight previous runtimes'
    evidence is kept.
  - Scope: this is runtime consistency, not isolation inside a session.
- **Codex threads.** A new process resumes the exact persisted thread under the current account
  and records the earlier accounts in `Identity.PriorChatGPTAccounts`. A resume target (a resumable
  identity, a legacy v0 identity, or a PTY-pinned id with no identity) is never replaced by an
  empty thread: a resume error fails the launch and leaves the identity unchanged.
- **PTY history.** Central Codex auth requires the App Server, so `codex.control pty` is reported
  and overridden. The decision follows the log's content. A log with structured events (anything
  other than an amux notice) is the session's history and is kept. Its rollout also holds the
  log's own turns, and log events carry no time to separate them. Otherwise, the rollout of the
  thread the next launch resumes (the identity's thread, including a legacy v0 identity, else the
  PTY-pinned id; this includes the agent's private Codex home) becomes the ordered prefix of the
  log. The prefix ends in a versioned marker notice, and notices already in the log follow it.
  - The rewrite runs under `Manager.WithQuiescentLog`, which holds the per-session start gate
    (no launch can begin), refuses while the session's runtime is running or starting, and
    excludes `AppendNotice`. Startup and every supervisor launch run it.
  - The outcome is recorded in `<log>.history.json`. If that write fails after the log was
    published, the retry recognizes the in-log marker and records the outcome instead of
    importing again.
  - Until then, a failed import refuses only that launch and is retried. An oversized line becomes
    an omission notice. The rollout is only read; it is the same file the App Server resumes.
- **Corrupt or insecure state.** A corrupt central Claude record never blocks binding or the
  repairing login, and delivery refuses it. An insecure store fails closed.

## Serialization audit (after #207, #212 and #214)

The launch fence decisions run inside one flock'd `accounts.Update` transaction: allocating,
retiring and pinning a launch; the superseded refusal; and the epoch, pending-login, blocked and
revision checks. Login start advances the delivery epoch and blocks delivery until the credential
commits, so a credential read that races a replacement fails the epoch check. Other guarantees still
rely on in-process ordering:

- Claude delivery (`dispatchCentralCredential`) holds `dispatchMu` and the exclusive `effectMu` around
  its authorization re-check, credential read and `Accept`. Account mutations and ceremonies
  (`accountCall`) also take the exclusive `effectMu`, so they remain mutually exclusive with it. Its
  archive/revocation re-check runs outside the account transaction and relies on that gate; splitting
  the gate would need the re-check moved into the admission callback, as Codex does.
- Codex delivery (`codexTokenSource`) deliberately takes no `effectMu`. Its authorization runs under
  the authority read lease inside `AcceptAuthorized`'s admission callback.
- #212's shared `admitSession` covers only session reports. Claude statusline quota observations are
  fenced by `ObserveClaude`'s own transaction (delivered epoch and version).
- #207's creation-readiness snapshot only short-circuits an all-ready preflight. It is also cleared on
  background account transitions and is never consulted by delivery.

## Regression tests

- `internal/accounts`: migration, idempotency, pending reporting, mode/API/account replacement,
  Claude idempotent retry and superseded refusal, forged/unallocated launches, kill/respawn race,
  bounded pending launches, logout before first delivery, insecure store.
- `internal/daemon`: the real admission path for Claude (`dispatchCentralCredential`: launch-less,
  forged, retry, replacement, kill/respawn in both orders) and for Codex (`codexTokenSource`:
  subscription → API with a real initial delivery, then superseded). Also `ensurePane`
  retire/keep decisions, corrupt-record repair login, PTY history import (order, idempotency,
  archived, notice-only log, structured-log evidence, v0 identity in the private home, recovery
  from a failed sidecar write, launch refusal then retry, bounded lines), and retired legacy defaults.
- `internal/codexapp`: a manager restart under a replacement account resumes the same thread;
  resume never falls back (resumable, v0 and PTY-pinned cases); a quiescent-log rewrite excludes
  notices and refuses a starting runtime.

Older captures under `docs/evidence/accounts-*` are dated records of the previous UI and keep its
wording.
