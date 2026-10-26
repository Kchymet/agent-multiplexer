# Auth follows central — work-in-progress checkpoint 2 (2026-10-25)

Branch `amux/355340-c0523b`; upstream merged through `b277792` (includes #202 `ca0411d`).
This responds to review b292d2 (`review/b292d2/REVIEW.md` at `2da36b3`), findings A1–A5,
and to root's feedback. Still WIP: some daemon and nativetui tests need updating (listed below).

## Native result (Codex 0.157.1, CI-pinned, sha256 verified) — asserted

`internal/codexapp/cross_account_resume_smoke_test.go` (opt-in `AMUX_TEST_CODEX_BIN`;
synthetic unsigned JWTs and a local TLS fake ChatGPT backend; it makes no real account
or network calls and copies no credential). PASS (29.5s). It asserts:

- Phase 1 runs as `acct-alpha`. Phase 2 is a new App Server that runs `thread/resume` on the
  same thread with `acct-bravo` tokens injected. The thread id is unchanged, and the request is sent
  with the `acct-bravo` header and no `previous_response_id`. Its `input` contains, in order,
  `user:PROMPT-ONE`, `reasoning:ENC-acct-alpha-1`, `assistant:answer 1`, `user:PROMPT-TWO`.
- Phase 3 runs as `acct-charlie`. The backend rejects the turn with `invalid_encrypted_content`.
  The turn fails, the identity keeps the same thread, and `thread/start` is never called.
- Phase 4 recovers on the same thread with the full ordered history plus `PROMPT-FOUR`.
- Within one process (reconnect), the bound account is still enforced: another account's token is refused.

Conclusion: continuing a thread under another account is supported natively. The earlier refusal
was amux policy. The real backend may still refuse foreign `encrypted_content`; a fake cannot
prove otherwise. If it does, that turn fails visibly and the thread and history are kept (phase 3).

## Implemented design (addresses A1–A5)

**Migration (A4).** `accounts.json` moves from v1 to v2 once, inside the existing flock. It is
idempotent by version. The v1 file is kept once as `accounts.v1.json` (`O_EXCL`). Unknown
or newer versions and malformed JSON are refused, and the file is left unchanged.
- Every default and binding (live, dormant, archived, console) becomes `<h>:central`.
- **No legacy fallback and no credential copying** (copying would fork rotating refresh chains;
  environment credentials are not amux's to move). The old source is kept only as evidence:
  `Binding.MigratedFrom`, `State.Migrated`.
- Until the first central login, Accounts, the CLI and `AccountSnapshot.migrationPending`
  report "migrated from X: central login required". Launches meanwhile use the existing
  central login flow. Running processes are not stopped (panes do not survive the daemon
  restart that installs the upgrade).
- New workloads are never seeded with a legacy source. Selecting a host, shared or
  environment default returns `account_legacy_source_retired`. `config set` accepts only
  central, and `unset` cannot bring back a legacy source.

**A3.** With no legacy fallback, `accounts.Mode` is constant (central) for the whole life of a
runtime, so the Keychain broker gate cannot change under a running process. A host-mode
runtime from before the upgrade cannot outlive the daemon restart that migrates.

**Process launch fence (A1; follows the refined review: no one-shot consumption).**
- Allocation happens only in the daemon: `accounts.BeginLaunch`.
  - Claude: in `panespec.centralLauncher` while the daemon resolves the agent-tab argv. The launch
    is passed as the launcher argv `--amux-launch=<seq>:<nonce>` (48 hex chars), which the exec into Claude replaces.
  - Codex: in `Manager.Ensure` on the fresh-process path only. The launch stays inside the daemon-side supervisor.
- Each binding has at most one pending launch. Every resolve before its first delivery reuses the same
  pending launch, so a reattach (a resolve whose pane was reused) neither builds up allocations nor
  displaces the one a waiting launcher holds.
- The first delivery for the pending seq and nonce pins the newest central mode, account and version.
- Repeated deliveries to the same launch (a timeout, a lost response, or an abandoned duplicate per
  R2) get the same snapshot. After a replacement login they get `account_runtime_superseded`
  ("restart to use the current login"), never the new credential.
- An older launch is superseded. A forged or unallocated seq, a wrong nonce, or a request with no
  launch is refused.
- The fences for epoch (logout, pending), state and binding revision are unchanged. A revision change
  during delivery returns a retryable `account_revision_changed`.
- Previous runtimes' delivered evidence is kept in `Binding.History` (at most 8 entries).
- Scope, as the review states it: this guarantees runtime consistency, not isolation from a malicious
  process inside the same session. Codex is stronger, because its launch never enters the sandbox.

**Codex thread bookkeeping (A5 and root point 2).** A new process does not use the
sidecar's account as a constraint. It resumes the same `ThreadID` and records earlier accounts
in `Identity.PriorChatGPTAccounts`, emitting a notice when the account changed.
`Config.ResumeRequired` is set for a resumable identity, or one with central account history, and
**disables the fallback to an empty thread**: a resume error fails the launch and leaves the identity
unchanged. Only legacy (v0) or never-run threads keep the missing-rollout fallback.
`account_rebind_requires_new_workload` is removed; `rebind` now just applies the current login immediately.

**Corrupt or insecure state (A2, C4).** An unreadable or corrupt central Claude record no longer fails
binding: bind, `AccountLogin` and logout proceed, delivery refuses with `central_credential_unavailable`,
and login replaces the record. An `ErrInsecureStore` still fails closed. Corrupt `accounts.json` is refused
and left byte-identical. Login, logout and default now bind every row in one `accounts.BindAll`
transaction instead of one fsync per row under `effectMu`.

**Wording.** CLI help and `config` help; TUI notices, confirmations and footer (the `d` default action is
removed; the old "Existing workload bindings stay fixed" line is gone); launcher guidance no longer mentions rebind.

## Overlap with 1a4fce (mailbox)

My changes touch `centralauth/launcher.go`: the optional `launch` request field, new
`fetchSuperseded` and `fetchChanged` guidance, and a `fetchCredentialFromSession(typed, launch)`
signature. They also touch `daemon/central_credential.go` (field validation, and passing the launch to `Accept`).
They do not change mailbox timing, budgets or publication. I will merge 1a4fce first and resolve conflicts on my side.

## Remaining

- Update the stale tests: `internal/accounts/api_mode_test.go` (the Launch type), the daemon account tests
  (migration, overview, fencing, rebind, `central_credential`), `centralauth` launcher guidance, nativetui
  account tests and evidence fixtures, and panespec central launcher argv.
- Add regressions: Claude same-launch retry after a lost response, then replacement leading to superseded,
  then a new launch delivering; a forged seq or a request with no launch refused; a corrupt record followed
  by login and a next-launch delivery; insecure store refused with the file unchanged; resume-required with
  a no-rollout error leaves the identity unchanged.
- Docs: `machine-accounts.md`, `broker-identity-spec.md`, `sandbox-config.md`, README, and CONTRIBUTING
  (prefer forward migration). Then `make test`, `make vet`, CI, and the draft PR.
