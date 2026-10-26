# Broker identity workgroup — independent review checkpoint (b292d2)

Reviewer: session b292d2 (Claude Opus 5.5), read-only. No production code was
edited; this directory is a temporary communication artifact, not for merge.

- Upstream reviewed: `3397deb` (#198); probes re-run unchanged at `4a485da`
  (#199, docs-only since `3397deb`). Go 1.27.3, linux/amd64.
- Author PRs: none published at time of writing (no `amux/355340-c0523b` or
  `amux/355340-1a4fce` branch on origin). Exact-head PR review still pending.
- Probes run via `go test -overlay` (no file added to any package). Sources are
  in this directory as `*.go.txt`; to run one, map it into the package with an
  overlay, e.g.
  `{"Replace":{"<repo>/internal/sessionrpc/zz_probe_test.go":"<path>/client_saturation_probe_test.go.txt"}}`
  then `go test -overlay overlay.json ./internal/sessionrpc -run TestProbeClientSaturation -v -count=1`.

Legend: **REPRODUCED** = demonstrated by a probe with a negative control;
**CODE-READ** = derived from source, not executed; **HYPOTHESIS** = plausible,
unproven.

---

## 1a4fce — protected-mailbox 10s Claude startup credential timeout

### R1 REPRODUCED — daemon-wide response budget uses per-subject constants
`newSessionResponseBudget()` (`internal/daemon/session_response_budget.go:44`)
passes `sessionrpc.MaxResponseFiles` (= `access.MaxQueuedRequests` = 128,
`internal/access/paths.go:22`), `MaxResponseBytes` (8 MiB, documented as the
*per-subject* bound, `internal/sessionrpc/protocol.go:53-55`) and
`MaxPendingReceipts` (128) as the **aggregate** limit shared by every session
server (`internal/daemon/session_lifecycle.go` `open`, `ServerOptions{ResponseBudget: r.responseBudget}`).
Responses are retained until `access.MaxResponseAge` = 60s
(`internal/access/paths.go:23`, `cleanupResponses` `internal/sessionrpc/server.go:700`);
the responses dir is client-read-only so clients cannot consume them. Ceiling:
~128 responses / 60s ≈ 2 req/s across all agents (hooks, statusline, events).

Reservation precedes verify/dispatch (`processClaimed` → `reserveResponse`,
`server.go:~398`); on failure `ServeOnce` returns `ErrResponseCapacity`, the
request stays in `processing/`, nothing is answered.

Probes (all pass at `4a485da`):

| Probe | Setup | Result |
|---|---|---|
| `daemon_budget_starvation_probe_test.go.txt` | production `newSessionResponseBudget()`, real `sessionrpc` servers, 4 busy subjects × 32 retained responses, then a new subject's request | `launcher dispatched=0 err=session RPC response capacity exceeded` |
| same, negative control | 127 retained | `launcher dispatched=1 err=<nil>` |
| `client_saturation_probe_test.go.txt` | **real `Client.Query`** (launcher shape, `central-credential`), aggregate budget limit = `MaxResponseFiles`, normal 20ms `ServeOnce` loop, 128 retained | first attempt: `session RPC outcome is indeterminate: context deadline exceeded after 3.001s` (launcher maps `DeadlineExceeded` → `fetchTimeout`, the 10s message); dispatch before expiry = 0; after +61s server clock, retry succeeds |
| same, negative control | 127 retained | first attempt OK in **41ms** |

This matches "first attempt times out at 10s, 5s retry succeeds": the retry is a
new request admitted once some retained response ages out. Tests that only
assert eventual start within 20s cannot catch it — **assert first-attempt
latency**.

### R2 REPRODUCED — timed-out (abandoned) request is still dispatched later
In the saturated client probe, total launcher dispatches = **2** for one
timed-out attempt plus one retry: after capacity frees, the abandoned request
in `processing/` is dispatched too. For `central-credential` that writes a
token response nobody reads (retained 60s), runs `accounts.Accept` side
effects, and consumes the freed slot ahead of the retry (amplifies starvation
under sustained load). Requests remain verifiable until `MaxRequestAge` = 30s.

### R3 REPRODUCED — a single subject can monopolize the whole daemon budget
`daemon_single_subject_probe_test.go.txt`: one subject with 128 retained
responses starves a different subject (`dispatched=0`); 127 → `dispatched=1`.
Per-subject `ensureResponseCapacity` cap (128, `server.go` `ensureResponseCapacity`)
equals the aggregate cap, so one noisy/buggy/compromised session is a
cross-session DoS. Error responses also count (reservation precedes `Verify`).

### C1 CODE-READ — receipts cost two retained slots and fail under saturation
A receipt is itself a request; `processClaimed` publishes a response for
`CallReceipt` (`server.go` `if call.Kind == CallReceipt { ... publishResponse }`).
Receipt-required operations therefore retain 2 files for 60s. When the budget
is full the receipt cannot be processed, so pending receipts settle as
`SettlementGraceExpired` instead of `SettlementReceipt`.

### C2 CODE-READ — post-restart admission blackout (not a deadlock)
Adoption with `Existing=true` bypasses limits and increments `files`
(`session_response_budget.go` `ReserveResponse`). If the prior boot retained
≥128 responses within 60s, the new daemon admits **no new response
daemon-wide** until they age out (≤60s) — exactly when restarted panes run the
credential launcher. Agree with root: no startup deadlock on current code.

### C3 CODE-READ — fairness
Freed slots go to whichever subject's pass reserves first (`serveCurrent`
starts concurrent passes); no FIFO/aging across subjects, so a waiting launcher
can lose to busy hook traffic repeatedly.

### C4 CODE-READ / HYPOTHESIS (secondary contributor) — global lock hold times
`dispatchCentralCredential` takes global `dispatchMu` + `effectMu`
(`internal/daemon/central_credential.go:63-66`). Same locks are held by
`reconcile` every 50ms while iterating all sessions (`session_lifecycle.go:195`,
`renewCurrent`/`open`), by host `handle` for whole actions incl.
`replaceSession` (`actions.go:67`), and by account operations; account
login/default/logout run one fsync'd `accounts.Update` per session row
(`BindExisting` loop, `internal/daemon/accounts.go` ~`AccountDefault||AccountLogin||AccountLogout`)
under `effectMu`. Credential delivery itself does 2 flock+fsync `accounts.Update`s
(`Delivery`→`BindExisting`, `Accept`). Not shown to cause the 10s timeout;
worth a latency regression.

### Options for safe consumption/retirement (author owns the fix)
Preserve: bounded storage, replay rejection, pending-receipt semantics,
authority/generation checks.
1. **Retire on receipt (smallest).** On accepted receipt, unlink the response
   and `Release` its lease (today only `ReleaseReceipt`). Do not publish a
   retained response for the receipt call itself (or retire it immediately).
   Replay safety holds: duplicate request IDs are rejected by the authority
   (`access.ErrReplay`, `internal/access/authority.go:54,966`), not by response
   retention.
2. **Consumption ack for idempotent queries**, or short retention for
   `RouteQuery` responses only; keep 60s/receipt for actions so a suspended
   client can still resolve an uncertain effect.
3. **Per-subject floor + per-subject cap < aggregate** (e.g. each live subject
   guaranteed ≥1–2 slots; aggregate = sessions × small cap). Storage stays
   bounded by session count.
4. **Separate quarantine accounting for prior-boot adopted responses** so they
   cannot block new admission (fixes C2).
5. **Signed client deadline (≤ `MaxRequestAge`)**; server refuses to dispatch
   past it, at least for `central-credential` (fixes R2).
6. **FIFO admission** across subjects rejected for capacity (fixes C3).

### Requested regressions for the 1a4fce PR
- First-attempt: aggregate budget saturated to 128 by *other* subjects → the
  launcher's **first** `FetchCredentialFromSession` succeeds within its 10s
  deadline (assert attempt count = 1, not just eventual start).
- Abandoned request past its deadline is never dispatched (dispatch count = 1).
- Single noisy subject cannot starve another (R3 probe inverted).
- Restart with ≥128 adopted responses still admits a new subject promptly.
- Receipt-required op: bounded files, settles via receipt under load.
- Negative controls: 127-retained baseline, replayed request rejected, revoked
  credential denied, archived session denied.

---

## c0523b — existing workloads follow current central auth (forward migration)

All CODE-READ against `3397deb`; to be verified at the PR's exact head.

1. **Pins to remove; snapshot vs durable binding.** `accounts.Binding` pins
   `CredentialVersion`, `ExpectedAccount`, `CredentialMode`
   (`internal/accounts/store.go`); `AcceptAuthorized` blocks on version/account/
   mode mismatch, so any re-login strands existing Claude sessions until
   `amux auth rebind`. Launcher guidance still says to rebind
   (`internal/centralauth/launcher.go` `fetchBlocked`). Follow-current must be
   decided at runtime start (`Ensure`/launch), while a **live runtime keeps its
   snapshot**: the Codex `PreviousAccountID` check (`codexauth.SessionToken`,
   `internal/daemon/central_codex.go` `codexTokenSource`) already fails closed
   and must stay. Regression: account A→B, restart → B without rebind; old
   runtime's refresh after the binding update → `ErrAccountChanged`, never B's
   token.
2. **Same-account refresh** (Codex) keeps working mid-turn. Profile switched to
   API while a subscription runtime runs → refresh fails with guidance, never
   adopts the API key (`profile.Mode=="api" && req.Refresh` errors today; API
   token with `AccountID==""` vs non-empty previous → `ErrAccountChanged`).
3. **Codex exact-thread resume.** `boundAccountFor`
   (`internal/codexapp/manager.go:~337`) seeds the old account from the
   identity sidecar, and rebind refuses `account_rebind_requires_new_workload`
   (`internal/daemon/accounts.go`). Restart after account/subscription↔API
   change must resume the **same thread id**; a resume error must not be
   classified as `isNoRollout` and silently start a fresh thread
   (`supervisor.go:~556`). In-daemon reconnect to the same App Server
   (`manager.go:110`) must keep the old account.
4. **Logout / pending / CAS / authority fences.** Logout fences durably first
   (epoch++, then `ClaudeStore().Delete()`); `Accept` must still compare
   `DeliveryEpoch` so logout between `Load` and `Accept` delivers nothing.
   Design question: `login-pending` currently blocks restarts while the old
   token remains valid — an interruption; decide explicitly. A migration-caused
   binding `Revision` CAS conflict should surface as retryable, not
   `account_binding_blocked`. Keep both `authority.Valid` checks around the read.
5. **Corrupt vs valid legacy.** `bind()` sets `CredentialUnknown=true` on every
   touched binding when the central store is unreadable (sticky). Regressions:
   corrupt store → no delivery; after fresh login → same session delivers with
   no rebind. Valid store → immediate delivery, generation preserved, no epoch
   bump, PR198 cached limits/budgets preserved. Migration idempotent (second run
   no-op).
6. **Legacy `host`/`shared` source** should switch only at restart. Do the
   migration in one `accounts.Update` for all rows, not per-row fsyncs under
   `effectMu` (see C4).

---

## Review criteria for the planned 1a4fce fix (signed one-way release + publish-before-launch)

Root reports author evidence at `d347440d` (6 subjects / 250ms → fresh
credential 53s; first fetch also misses service publication → 5s). For the fix:
- **Release priority:** a release must be admitted even when an earlier ordinary
  queued request in the same mailbox fails capacity (`processClaimed` returning
  `ErrResponseCapacity` currently aborts the whole `ServeOnce` pass, so a release
  queued behind it would never run). A release must not itself need a response
  reservation.
- **Authenticate the release:** signed by the current principal; bind
  request ID + response digest + subject + credential generation; unknown /
  mismatched / stale-generation release is a no-op. A release must not retire a
  response whose receipt is still pending (or must equal that receipt).
- **Bounded:** releases count toward `MaxQueuedRequests`; replayed release
  rejected by `access.ErrReplay`; no unbounded tombstones.
- **Tests use the production client** (`Client.Do`/`Query` path that sends the
  release), not a stat-only helper; an old client that never releases must
  still be bounded by 60s retention (compat negative control).
- **First-attempt latency** asserted (attempt count 1, latency well under 10s),
  not eventual success. Publish-before-launch: the launcher must not start
  before `PublishService` + `Initialize` for its mailbox; assert no 5s retry.
- **No lifecycle regressions:** no duplicate mailbox owner across
  close/retire/reopen, no lock-order cycle among `dispatchMu`/`effectMu`/
  `serveMu`/budget mutex, reconcile/init/shutdown joins still terminate.

---

## Independent re-run of author evidence `d347440d` (1a4fce WIP, no fix)

Author's opt-in harnesses mapped via `go test -overlay` onto upstream `4a485da`
(author commit adds only those two test files + `docs/session-mailbox-pressure.md`).

| Harness | Condition | Result (b292d2 run) |
|---|---|---|
| `TestMeasureFirstDelivery` (`AMUX_MEASURE_FIRST=1`) | no pressure, no hold | **REPRODUCED** delivered after **5.068s**; launcher printed `fetchTransport` ("request failed"), i.e. first fetch misses unpublished `service.json`, then 5s `LaunchRetry` |
| `TestMeasureMailboxPressure` 250ms × 6 subjects | pressure | **REPRODUCED** budget `files=128 bytes=82688 receipts=0`; credential after **53.26s**; launcher printed transport failure **then** the 10s timeout message; background 148 ok / 6 timed out, max 18.8s |
| same, 5s × 6 subjects | negative control | budget `files=12`; credential after **5.07s** (publication miss only); background 18 ok / 0 timed out |

Conclusions: two independent causes, both reproduced — (a) publication miss
gives a deterministic ~5s first-start delay on every fresh session; (b) the
128/60s global pool produces the 10s timeouts and tens-of-seconds delivery under
ordinary multi-agent traffic.

Notes on the harness for the fix PR:
- `measureSignedCall` is a stat-only helper that never consumes/releases; after
  a release-based fix it will still behave like an old client. Keep it only as
  the **old-client compat control** (must stay bounded by 60s retention); the
  pass/fail regression must use the production `sessionrpc.Client` /
  `centralauth.FetchCredentialFromSession` path.
- Convert both to non-opt-in assertions: fresh-session credential delivered on
  the **first** attempt (no launcher guidance printed), latency ≪ 5s, and under
  the 250ms × 6 load latency ≪ 10s.
- Launcher misclassification: the publication miss surfaces as
  "request failed … check daemon status", which misdirects users; with
  publish-before-launch this path should be unreachable, but a not-yet-published
  service is better reported/retried as "starting" with a short backoff.

---

## c0523b WIP checkpoint `1628b9a` — review (2026-10-25)

Read `docs/evidence/auth-follow-central/CHECKPOINT.md` and the full diff vs
`3397deb`. Probe: `c0523b_auth_follow_probe_test.go.txt` (daemon package, via
overlay). WIP daemon tests do not compile yet (`account_overview_test.go:53`
compares a struct now containing `[]accounts.Delivered`); for the probe run that
file (and any other stale test) was overlaid with an empty `package daemon`
file. Negative control = same probe on upstream `3397deb`/`4a485da`.

### BLOCKER A1 — REPRODUCED: live Claude principal obtains changed auth (next-process-only broken)
`dispatchCentralCredential` (`internal/daemon/central_credential.go`, unchanged)
calls `accounts.Accept(...)` **without a launch**, and `AcceptAuthorized` treats
a launch-less call as `allocate` → a new launch on **every** query
(`internal/accounts/store.go` `if allocate { b.Launches = ...+1; seq = b.Launches }`).
The "current launch keeps mode/account/version" branch is therefore unreachable
for Claude. Any process holding the session principal (the running agent, or
the session's shell tab — both can sign mailbox requests) can re-issue
`central-credential` after a replacement login and receive the new credential
(including a different account, or an API key via `credential-format=typed-v1`).

| Run | Result |
|---|---|
| WIP `1628b9a` | first gen=1; same principal after `completeClaude` replacement → `status=ok gen=2 changedToken=true` (**LEAK**) |
| upstream control | same sequence → `status=denied code=account_binding_blocked` |

Fix direction: allocate the Claude launch on the daemon side when the agent tab
process is started (`replaceSession`/start → `engine.Ensure`), hand the launcher a
single-use launch identifier only in the agent tab's launch, and make
`dispatchCentralCredential` require it and **consume** it (Claude never
refreshes, so one delivery per launch). A launch-less request, a reused launch,
or another tab's request is refused. Regression: probe above must return
non-OK for the second query, while a real restart (new launch) gets gen=2.

### BLOCKER A2 — REPRODUCED: corrupt central record blocks the repairing login
`bind()` now `return err` when `CredentialVersion[claude:central]==0` and
`ClaudeStore().Load()` fails with anything but `ErrNotConfigured`. `accountCall`
runs `BindExisting` for every row before `AccountLogin`/`Logout`/`Default`, so
login fails with `account_binding_invalid`.

| Run | Result |
|---|---|
| WIP, corrupt `credential.json` (pre-feature, never bootstrapped) | `login begin ok=false error="account_binding_invalid"` |
| WIP, valid store (control) | `ok=true` |
| upstream, corrupt | `ok=true` (CredentialUnknown path) |

CODE-READ consequence: the same error also fails `Delivery` and new Claude
`Bind`/`BeginLaunch` for every session until the file is hand-repaired. Fix:
treat an unreadable record as "not configured yet" for bootstrap (deliver
nothing, `central_credential_unavailable`), never fail binding; `SaveKind`
already handles a corrupt prior record. Regression: corrupt → no delivery →
login completes → same session's next launch delivers, no rebind; plus valid
control with generation unchanged and no epoch bump.

### HIGH A3 — CODE-READ: first central login interrupts running legacy-host Claude on macOS
`daemon/credential_broker.go:34` gates Keychain broker ops per request with
`claudecfg.HostStoreSharedFor(principal)` → `accounts.Mode(id)`, and
`normalize()` clears every `LegacySource` the moment central becomes
configured. A host-mode Claude process launched before the login keeps running
but its Keychain reads/refresh writes are now refused mid-turn — contradicting
"running processes keep what they received". Seatbelt profile
(`panespec/seatbelt.go:300`) is launch-time so only the broker gate is live.
Fix: gate the broker on the runtime's launch-time source snapshot (record it with
the launch), not on the durable binding. Regression (darwin or broker-level
unit): host-mode runtime launched, central login completes, broker read still
allowed for that runtime; new launch is central and broker refused.

### MEDIUM A4 — LegacySource is indefinite
Legacy retires only on the first central login. `LegacyUsable` returns true
unconditionally for `environment` (both harnesses) and for `host` on darwin, and
`bind()` seeds **new** workloads from `LegacyFallback`, so an installation that
never logs in keeps creating legacy workloads forever. That is the indefinite
compatibility mode the user asked to avoid. Suggest: legacy only for bindings
that existed at the v1→v2 migration (no `LegacyFallback` seeding of new
workloads), a persistent "migration pending — run `amux auth login <h>`" status
in accounts/TUI, and keep "retire at next launch after first central login".
Also: `amux config set claude.auth host|shared|environment` now errors; confirm
that `unset` cannot resurrect a legacy fallback.

### MEDIUM A5 — exact-thread fallback remains substring-based and unconditional
`supervisor.go` `if err != nil && isNoRollout(err) { resumed=false; start() }`
fires for any `thread/resume` error containing "no rollout", including
`Identity.Resumable == true` threads that are known to have history. With
routine cross-account resumes this path matters more. Gate the fallback to
threads not known resumable (legacy v0 / never-run), and for a resumable thread
fail the launch visibly with the identity unchanged. Regression: fake App
Server returns a "no rollout"-shaped error for a resumable thread → launch
error, `ThreadID` unchanged, no `thread/start`. Checkpoint's residual risk
(backend rejecting another account's `encrypted_content`) is a turn-time error,
not a resume error — add an assertion that such a turn failure does not rewrite
identity or start a fresh thread.

### Verified OK by reading (keep as regressions)
- Codex old runtime: `req.Launch < binding.Launch` → `ErrSuperseded`; refresh
  only for `req.Launch == binding.Launch`; same-launch account change →
  `ErrAccountChanged` (`central_codex.go`). `BeginLaunch` only on the
  fresh-process path (`existingForEnsure` returns before it).
- Logout/pending epoch fences unchanged in `AcceptAuthorized`.
- v1→v2 migration idempotent by version; `accounts.v1.json` written once with
  `O_EXCL`; unknown/newer version refused without rewrite.
- Minor: `migrateV1` bumps every binding's `Revision`; an in-flight delivery
  across the migration gets `account_binding_blocked` (should map to a
  retryable code, launcher text still mentions rebind semantics).
