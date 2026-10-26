# Auth follows central — work-in-progress checkpoint (2026-10-25)

Branch `amux/355340-c0523b`. Incomplete: tests outside `internal/accounts` and
`internal/codexapp` are not yet updated, and the TUI, docs and identity spec are pending.

## Native result (Codex 0.157.1, CI-pinned, sha256 verified)

`internal/codexapp/cross_account_resume_smoke_test.go` (opt-in, `AMUX_TEST_CODEX_BIN`)
uses synthetic unsigned JWTs and a local TLS fake of the ChatGPT backend. It makes no
real account or network calls and never copies a credential.

1. The App Server is started with externally managed tokens for `acct-alpha` and runs one turn.
   The fake returns a reasoning item with `encrypted_content: ENC-acct-alpha-1`.
2. The App Server is stopped. A new one is started with `thread/resume` on the same
   thread id while `acct-bravo` tokens are injected.
3. Result: the server accepts B's login, the thread id is unchanged, and the model
   request is sent with `chatgpt-account-id: acct-bravo`. Its `input` holds the
   full phase-1 history, including A's reasoning item and its `encrypted_content`.
   There is no `previous_response_id`.
4. When the old account is kept as a bound constraint, amux refuses the restart
   (`errAccountChanged`). That refusal was amux policy, not something Codex requires.

Residual risk: the real backend might refuse another account's
`encrypted_content`. A fake cannot settle that. If it happens, the turn fails
visibly, and the thread, its rollout and the existing fallback to an empty thread remain.

## Chosen design

- **Store**: `accounts.json` v1 → v2. The migration runs under the existing flock and is idempotent by
  version. The original v1 file is kept once as `accounts.v1.json` before the
  first v2 write. Unknown or newer versions, or malformed metadata, give an explicit
  error and nothing is rewritten.
- **Bindings**: every binding (live, dormant, archived, console) points at
  `<h>:central`. Defaults are always central. Selecting a host, shared or
  environment default returns `account_legacy_source_retired`.
- **Legacy credentials**: these are not copied into central. Host and shared logins
  hold rotating refresh tokens, and a copy would fork the chain. Environment,
  Bedrock and apiKeyHelper credentials have nothing that can be copied. Instead,
  while central has never been configured, a migrated workload keeps
  its `legacySource` if that login is still usable (`LegacyUsable` checks:
  host credential file, shared enabled, environment). Otherwise it uses central
  and the existing login flow. The first central login permanently clears every
  `legacySource` for that harness (`State.normalize`).
- **Next-launch refresh**: Codex: `Manager.Ensure` on the fresh-process path
  calls `accounts.BeginLaunch`, and the sequence goes into every `TokenRequest`.
  Claude: each launcher delivery is one exec, so it allocates its own
  launch. A new launch pins the newest central mode, account and version. The
  previous runtime's evidence moves to `binding.history` (at most 8 entries).
- **Active runtimes**: deliveries to the current launch must keep the same mode
  and account (same-account Codex refresh continues; Claude and API keep the same version).
  Deliveries for an older launch return `account_runtime_superseded`. The epoch
  fences for logout, pending, cancellation and revision are unchanged. Login stops nothing.
- **Thread bookkeeping**: a new Codex process no longer passes the sidecar's account as a
  constraint. It resumes the same `ThreadID` and records earlier accounts in
  `Identity.PriorChatGPTAccounts`. `account_rebind_requires_new_workload` is
  removed, and `rebind` is now an optional "apply now".

## Remaining

Update the existing daemon, nativetui and panespec tests. Add regression tests for
replacements, mode and account changes, exact-thread continuity, migration
idempotency, and old-runtime races. Then: TUI wording (remove "Existing workload
bindings stay fixed" and "Use for new workloads"), `machine-accounts.md`, the
broker identity spec, `sandbox-config.md`, the CONTRIBUTING migration preference,
and a full `make test`, `make vet` and CI run.
