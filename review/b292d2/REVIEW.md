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

---

## Refinement (after root feedback): A1 launch-fix design and A2 scope

### A1 — do NOT use a consume-on-first-delivery nonce
A one-shot consumed before the client observes the response breaks exactly the
cases reproduced for 1a4fce: client timeout (R1), lost/expired response,
daemon restart between response and read (`ErrRestarted`), and the abandoned
duplicate dispatch (R2). The launcher would then retry into a permanent denial.

Safe shape (reuses the WIP's existing same-launch branch; no one-shot packet):
1. **Daemon-only allocation.** `BeginLaunch` runs on the host side when the
   agent tab process is actually started (`replaceSession`/start →
   `engine.Ensure`), never from a mailbox request. Start failure retires the
   allocation (so an old process cannot claim an allocated-but-unused launch).
2. **Pin on first delivery, idempotent re-delivery.** A Claude request carries
   its launch; `AcceptAuthorized(..., launch)` with `seq == b.Launch` re-delivers
   only if mode, account and **version/generation are unchanged** and the epoch
   is still current (this branch already exists). Retries after a timeout or a
   lost response get the same snapshot. After a replacement login the same launch
   gets a stable refusal (`account_runtime_superseded` / "restart to use the new
   login"), never the new token. Logout/pending epoch fences still apply.
3. **Bound the claim.** Today the `default:` branch accepts any `seq > b.Launch`
   and sets `Launches = max(Launches, seq)`. With a client-supplied launch that
   is a bypass: an agent sends `b.Launch+1` and pins the replacement. Require
   `b.Launch < seq <= b.Launches` **and** that `seq` is the single outstanding
   daemon allocation. Launch-less Claude requests are refused (remove the
   `allocate` path for Claude once the launcher sends the launch).
4. **Identifier.** Bind the allocation to a random per-launch value (≥128 bits)
   given only to the agent tab's launcher; the launcher strips it from the
   environment before `exec`. A sequence number alone is guessable.

**Actual protected scope (be explicit in the PR).** All tabs of a session share
one session principal (`central_credential.go` comment), the same uid and the
same sandbox. A same-session process can read the launcher's environment
before it execs, and the agent may legitimately request its own restart. So the
launch fence is a **runtime-consistency guarantee**: a running process, an old
process, or a launch-less/other-tab request cannot adopt a replacement login.
It is **not** isolation against a malicious agent inside its own session. That
is enough for the user's next-process-only promise. Knowing the current launch
id yields only the snapshot the process already holds in
`CLAUDE_CODE_OAUTH_TOKEN`, so the id does not need to be a strong secret. It
only needs to be unforgeable for *future* launches (item 3). Codex is stronger:
its launch lives in the daemon-side supervisor and never enters the sandbox.

Regressions:
- Same launch: first delivery, then response lost (do not read the response),
  then retry → same token and generation.
- Replacement login, then the same launch → refused with no token. A new
  daemon-started launch → new token.
- Forged `b.Launch+1` and launch-less request → refused.
- Abandoned duplicate (R2 shape) → same snapshot, no new pin.
- Logout between allocate and first delivery → refused.
- Old process after a new launch pinned → superseded.

### A2 — distinguish three failure classes; only one may admit repair login
Current `centralauth.Store.read()` returns `ErrNotConfigured`, a
**`ErrInsecureStore`-wrapped** error (symlink, wrong type, mode, owner), or a
**plain** `fmt.Errorf` for "unreadable credential record" / "unsupported
credential record" (decode, version, harness, kind). An invalid stored value
wraps `ErrInvalidSecret`. Recommend a sentinel `centralauth.ErrCorruptRecord`
for the decode/version/invalid-value cases.

| Class | Bind/bootstrap | Delivery | Host login |
|---|---|---|---|
| Corrupt **credential record** (private, regular file, bad content) | skip bootstrap, bind OK | refuse (`central_credential_unavailable`), never deliver | **allowed**: `SaveKind` already replaces it (generation falls back to wall clock, never reused) |
| **Insecure store** (`ErrInsecureStore`) | fail closed | refuse | refuse; `SaveKind`→`ensureDir`/`checkPrivate` already refuses. User fixes permissions. Do not weaken. |
| Corrupt **accounts metadata** (`account_store_invalid`, `_version_unsupported`, `_insecure`) | fail closed, never reinitialize/rewrite (WIP already does this) | refuse | refuse until repaired; `accounts.v1.json` backup aids manual repair |

Regressions:
- Corrupt record: bind OK, delivery refused, login completes, the same session's
  next launch delivers with no rebind. Generation is greater than any prior.
- Insecure dir or file (mode 0644, symlink): login and delivery both refused, and
  the file is unchanged.
- Corrupt `accounts.json`: every operation refused and the file byte-identical
  afterwards.
- Valid control: delivery works, generation unchanged, no epoch bump.

---

## 1a4fce mailbox checkpoint `964ddfa` — review (PRIORITY)

Base `d347440`; fix = one-way `CallRelease`, release processed behind a
capacity-blocked request, receipt without capacity, prior-boot retirement at
adoption, launcher waits for publication within the 10s attempt. Author tests
pass (`go test ./internal/sessionrpc ./internal/centralauth`). Probes below run
via `go test -overlay`. `mbx_sessionrpc_probe_export.go.txt` overlays a
non-test `ProbeOpenClientAt` so a daemon-package probe can use the
**production** `sessionrpc.Client`. Pre-fix control tree = upstream (sessionrpc
unchanged since `3397deb`).

### M-OK1 REPRODUCED fix: production clients + real daemon + fresh native credential
`mbx_prodclient_daemon_pressure_probe_test.go.txt` = the author's real-runtime
harness (6 subjects × 250ms, real protected-namespace Claude launcher, real
global budget). Background traffic is replaced by hook-shaped pairs
(`report-context` + `version`) through the production client.

| Tree | budget after warmup | fresh credential | launcher guidance | background |
|---|---|---|---|---|
| `964ddfa` | files=0 bytes=0 | **117.9ms, first attempt** | none | 138 ok / 0 timed out, max 134ms |
| pre-fix control | files=128 | 53.1s | 10s timeout message | 81 ok / 6 timed out, max 21.7s |

Also, my earlier real-client saturation probe now reports `busy=128 first=<nil>
after 21ms`: the 128 busy queries were released. The probe's own assertion
"expected starvation" therefore fails on `964ddfa`, which is the intended
result. `TestMeasureFirstDelivery`: 114.6ms with no guidance (was 5.07s). The
author's stat-only harness still gives files=128 / 54.4s, correctly as the
**old-client compat control**.

### M1 REPRODUCED (root finding, author ACKed): receipt responses still retained
`mbx_receipt_completion_probe_test.go.txt` `TestProbeReceiptResponseRetention`:
production client, 8 mandatory-receipt actions, pool limit 4, server drained
after each → **3 response files retained** (all receipt responses; settlements
all `receipt`). Control with 8 ordinary ops (release path) → **0 retained**. The
receipt's own `.res` is published whenever capacity exists
(`processClaimed` `CallReceipt` → `publishResponse`), `sendReceipt`/`sendAck`
never reads it, and nothing releases it, so each mandatory op holds one slot
for 60s. Only once the pool is full does the `discard` branch apply.
**Fix: make receipts always one-way like release.** Reserve nothing and
publish nothing; `Verify` still consumes the replay ledger; receipt status
stays internal (a client never reads it). Regression: the probe must retain 0.

### M2 REPRODUCED: prior-boot retirement turns committed success into ErrRestarted+ErrIndeterminate
`priorBoot`'s rationale "Clients reject such responses" is **false**.
`Client.awaitResponse` calls `readResponse(credential, service, request)`
against the **original** service, whose boot ID matches the old-boot response,
before it compares the current boot. `sendReceipt` documents that "A restart
after receiving a durable response does not invalidate that success".
Suspended-client probes (client polls every 1.5s; old boot commits; restart;
new boot `Initialize` adopts):

| Probe | pre-fix | `964ddfa` |
|---|---|---|
| ordinary action (`mbx_oldboot_probe_test.go.txt`) | `ok "committed"`, dispatches=1 | `ErrIndeterminate`+`ErrRestarted`, dispatches=1 |
| mandatory-receipt completion | `ok "archived"`, dispatches=1, settlement `server_closed` | `ErrIndeterminate`+`ErrRestarted`, dispatches=1, settlement `server_closed` |
| no-restart control | `ok` | `ok` |

Safety properties hold: exactly one effect, no automatic replay (dispatch stays
1 after further passes), completion settlement unchanged, and a client that
already read its response keeps it. What is lost is observation of a committed
success, including `agent done`-style completions, which invites a
*manual* duplicate retry. The existing `TestInFlightRestartIsIndeterminateAndNotRetried`
covers only a zero-effect request.

Verdict: acceptable **only** as documented, bounded uncertainty under root's
criteria. Required:
(a) Correct the `priorBoot` comment and the docs: old clients can read
    old-boot responses, and cleanup deliberately trades that for capacity.
(b) Add the committed-effect regression above: exactly 1 dispatch,
    `ErrRestarted`+`ErrIndeterminate`, no replay.
(c) Preferably **exempt `ReceiptRequired` (completion) responses**, or account
    prior-boot responses in a separate bounded pool that does not block new
    admission (they are already bounded on disk). That fixes the post-restart
    blackout without losing durable completion.
Note: a release of an old-boot response after restart is refused today
(`retireResponse` requires `BootID == current`, and the client signs with the
original service boot), so age cleanup is the only other retirement path.

### Other review points (CODE-READ)
- Release routing on the unverified kind is safe: `processRelease` re-verifies
  (replay ledger consumed), checks the subject, then `Valid`, then an exact
  digest/key/generation/boot match. It never retires a pending-receipt or
  `ReceiptRequired` response. A forged release is a no-op and is deleted.
- `blocked` keeps scanning within a pass, so a release behind a
  capacity-blocked request is processed. Non-release requests with
  `admit=false` stay in `processing/`, unverified and undispatched. Good.
- Each op now sends 2 request files (op + release), so queue and replay-ledger
  load roughly doubles. The 128 queue bound still applies; consider whether
  the ledger has a bound.
- `ErrUnpublished` (service.json `ErrNotExist`) also covers a retiring or
  archived mailbox. The launcher then waits the full 10s before guidance. That
  is acceptable.

---

## c0523b auth checkpoint `400c151` — review of A1–A5 fixes

Read `docs/evidence/auth-follow-central/CHECKPOINT.md` (checkpoint 2). Stale
tests are known WIP, so for the probe every `internal/accounts/*_test.go` was
overlaid empty. Probe: `c0523b_launch_fence_probe_test.go.txt`.

### Launch fence — REPRODUCED behaviour (accounts level, Claude shape)
- Same launch, retry after a lost response → same snapshot accepted. Good.
- Running P1 after a replacement login (v2) → `ErrSuperseded`. Good.
- Forged next seq without a nonce, launch-less, wrong nonce → refused. Good.
- Two reattach resolves while P1 is alive → one pending launch reused
  (`launches=2`), no accumulation. Good.
- **A1-R1 HAZARD (liveness, not credential leak):** a pending launch is reused
  by *every* resolve until its first delivery, including a user restart's new
  process. If a killed or dying process P2 that was spawned with that pending
  argv delivers first, and a replacement (v3) lands before the new P3's first
  fetch, then **P3 — a freshly started process — gets `ErrSuperseded`**.
  Reproduced: `new process P3 sharing reused pending launch after replacement:
  err=account_runtime_superseded`. A further restart recovers, because pending
  is then nil, but the user's "restart to use the new login" is defeated
  once. Fix: allocate or bind the launch at actual spawn (engine `spawn` under
  `e.mu`, only when no live instance), or retire the pending launch when
  `replaceSession`/auth-reload kills the old instance, so a kill-and-respawn
  always gets a fresh launch.

### Actual runtime boundary of the pending nonce (CODE-READ)
- Allocation happens at argv resolve (`panespec.centralLauncher`), not at
  spawn. `paneOpen` resolves on **every attach**, even when
  `engine.Ensure` returns the live instance (`local.go` Ensure:
  `if alive { return in }`). After each delivery the next attach therefore
  allocates a new pending launch whose nonce exists only in a discarded argv,
  in daemon memory, and in plaintext in `accounts.json` (`Pending.Nonce`,
  under AuthDir, which is not mounted in panes). I found no path that hands
  a discarded argv to a running process: the engine never re-spawns from a
  stored spec. So there is **no future-account permit reachable by a running
  process**. The cost is one fsynced accounts write per pane attach.
- Linux: the agent pane runs under `bwrap --die-with-parent --unshare-pid`.
  The nonce is in launcher argv only inside that pane's own pid namespace
  until `exec` replaces it, and other tabs cannot see it.
- macOS (seatbelt, no pid namespace): while a launcher waits (login pending,
  mailbox down), the nonce is visible via `ps` to same-uid processes,
  including the session's shell tab. A shell tab claiming the pending launch
  only gets what the launcher would have received, which is consistent with
  the stated consistency-not-isolation scope. Document this in the PR.

### A3 / migration boundary (CODE-READ) — holds, with caveats
`accounts.Mode` is now constant central, and nothing in `cmd/` writes
`accounts.json` (all writers are daemon-side), so the v1→v2 migration happens
under the new daemon. Linux panes use `--die-with-parent`, and engine Shutdown
terminates instances on restart. Caveats: (1) on macOS a SIGKILLed old daemon
leaves seatbelt panes to the pty hangup only. (2) An **old** daemon reading a
v2 store gets `account_store_invalid` (its `read()` requires Version 1), so
any downgrade or rollback blocks delivery. Keep the `accounts.v1.json`
restore note in the docs.

### A5 / unknown legacy history (CODE-READ) — partially fixed
`resumeRequiredFor` = identity present && same thread && (`Resumable` ||
`ChatGPTAccount` || `PriorChatGPTAccounts`). It is **false** for (a) a v0
legacy identity without central account (`resumeThreadFor` still resumes it)
and (b) a legacy PTY thread id from the store with no identity file
(`legacyThreadID`). Both may hold real history, yet still get the
"no rollout" → fresh-thread fallback, i.e. unknown history is treated as
empty. Ask: treat unknown legacy (v0 / no-identity with a non-empty thread
id) as resume-required, or fall back only after positively establishing
absence (no rollout for that thread id in the agent's `CODEX_HOME/sessions`,
and in any older home layout earlier amux versions used). Otherwise fail
visibly with an explicit "start fresh" action. Regression: v0 identity and
no-identity legacy thread, plus a "no rollout" resume error → launch fails,
identity/thread unchanged, no `thread/start`.

### A2 / A4 — resolved as designed (CODE-READ)
`bind` skips bootstrap on a corrupt record and fails only on
`ErrInsecureStore`. `BindAll` is one transaction. There is no legacy fallback
for new workloads, and `config set` accepts only central. Re-run my A2 probe
once the daemon tests compile.

---

## PR206 final exact-head review `e3cdeea` — SIGNOFF (CI-conditional)

Posted on PR206 (issuecomment-6407449941). The only production delta since
`08af41b` is the nil-by-default `beforeResponseRead` test hook. Targeted
negative controls, overlaid into the head `server.go`:
- **Mutation A** (conditional receipt discard): `RepeatedMandatoryReceipts/pool=4`
  fails with `active=1 files=1`; `pool=1` alone would pass.
- **Mutation B** (prior-boot deletion): the restart test fails at the
  client-outcome assertion.
- The barrier test passed `-race -count=30`. `sessionrpc` and `centralauth`
  pass under race.

---

## c0523b `1272cfb` — test migration review (auth final review resumes after mailbox merge)

`1272cfb` changes tests plus `amuxcfg/config.go` (App Server control is now
forced for every Codex workload). **No implementation change** to
`resumeRequiredFor` (`codexapp/manager.go:347`) or to pending-launch reuse
(`accounts/store.go:459`). A5 and A1-R1 are therefore not settled.

Suite status at `1272cfb`: accounts, codexapp, amuxcfg and daemon pass;
**`centralauth` fails** (`TestLaunchClassifiesFailuresAndRecovers`, stale
launcher guidance — known WIP).

### A5 — REPRODUCED at `1272cfb`: unknown legacy history is still assumed empty
`c0523b_a5_unknown_legacy_probe_test.go.txt` (codexapp, real
`resumeThreadFor`/`resumeRequiredFor` + `fakeServer` with
`resumeErr: "no rollout found"`):

| Identity | resumeRequired | Outcome |
|---|---|---|
| none (legacy PTY thread id from the store) | false | **fresh `thread/start`, thread replaced by `thr_1`** |
| v0, no central account | false | **fresh `thread/start`, thread replaced by `thr_1`** |
| control: resumable v1 | true | launch fails, no `thread/start` |

`1272cfb` forces App Server control for all Codex workloads, so every legacy
PTY conversation now passes through this predicate. The new
`TestHandshakeResumeRequiredNeverFallsBack` covers only the already-resumable
case.

### Legacy PTY → App Server migration (root focus, folded into A5)
- **Same private home: CONFIRMED (CODE-READ).** PTY `Resolve` and
  `AppServerCommand` both build through `wsops.AgentCommand` → harness
  `Config` → `codexcfg.Template(s.ID, s.Dir)`, i.e. the same
  `codexcfg.AgentHome(s.Dir)`. A structured `thread/resume` therefore looks
  for the legacy rollout in the home where PTY Codex wrote it.
- **Cold history: NOT preserved through amux surfaces (REPRODUCED).**
  `c0523b_legacy_pty_cold_history_probe_test.go.txt` puts a real PTY rollout
  in the agent's private `CODEX_HOME`. The harness still resolves it
  (`RuntimeTranscriptPath` == rollout), but `RuntimeRecord` returns
  `Structured=true`, path `…/codexapp/c1.events.jsonl`, and **that file does not
  exist**. `sessionEventSourcesForTracked` uses only the structured event log
  (`session_events.go:~841`), and nothing in `codexapp` backfills pre-migration
  turns into it. Cold readers (provider transcript, `amux agent events`) see
  **none** of the pre-migration PTY history; only native Codex (resume/TUI)
  sees it.
- The changed `query_test` removed exactly the PTY-rollout fixture that would
  show this, and now asserts only the event-log path, so it no longer proves
  history is accessible.
- Same behaviour on upstream for central-mode PTY-pinned sessions: it
  **predates** this branch, but the migration extends it to every legacy host
  PTY session.
- Ask: for a structured session with no structured log yet (or with legacy
  turns), either include the legacy rollout as an earlier read-only source in
  the runtime record / event sources, or backfill on first resume. Then
  restore a fixture with a real rollout and assert its content is readable.
  Also make legacy resume required (A5) so a "no rollout"-shaped error can
  never discard it.

### A1-R1 pending-launch reuse — production gates checked → LOW
Gates checked:
- `replaceSession` runs under `d.handle`, holding `dispatchMu`+`effectMu`
  for resolve → kill → spawn.
- Engine kill is synchronous (`instance.kill` → `terminateReporting`,
  SIGTERM→SIGKILL, wait).
- Claude credential dispatch also needs `effectMu`.

So the old process cannot deliver between resolve and spawn. The only
remaining window: the old process's request was already queued (the R2
shape, still dispatched after it died), it is dispatched after the restart
and pins the reused launch, and a login completion (also under `effectMu`)
lands before the new process's first dispatch. The new process then gets
`account_runtime_superseded`. This is a liveness problem only, it heals on
the next restart, and the window is a few ms between two queued dispatches.
The accounts-level probe shows that the state machine allows it; it is
**not** proof that it happens in production. Recommended cheap hardening,
non-blocking: retire `Pending` when the instance that held it is
killed/replaced, or allocate at spawn.

### Weakened acceptance in the test migration (review of the actual test diffs)
- `credential_broker_test`: dropped the revoked-session negative and the
  served-call count, and left `_ = value; _ = d` dead code. Claude broker ops
  are now always denied, so the revocation check is moot for Claude. The
  Codex revocation path at line 62 remains. Clean up the dead code; keep one
  assertion that the broker callback is never invoked (`calls == 0` is
  asserted per verb, OK).
- `control_test`: removed the PTY/AppServer frozen-routing matrix and
  `TestSavedOptInDoesNotRerouteRunningPTY`. That is acceptable, because PTY
  can no longer be effective and control stays frozen per daemon lifetime.
  The new test asserts that a fresh daemon keeps AppServer and warns.
- `query_test`: see cold history above. This is the one weakening that hides
  a real behaviour gap.
- `account_migration_unknown_test.go` deleted (CredentialUnknown no longer
  exists). The replacement coverage for corrupt-record repair should be the
  A2 regression (corrupt record → login → next-launch delivery; insecure
  store refused, file unchanged). Confirm it exists before final review.

---

## PR206 macOS CI failures (`e3cdeea`, `bd3a6cb`) — signoff SUSPENDED

Posted as PR206 issuecomment-6407618479.
- **`e3cdeea`: `pressure-2 39/40`.** The timestamps show a genuine stall
  (more than 160 fetches in about 3s, then about 87s without progress). No
  `session RPC pressure-2` errors were logged. The failure splits into
  fixture-exit vs alive-and-retrying. The `bd3a6cb` diagnostics separate those
  two; still missing: diagnostics on the warmup and fresh failure paths,
  server-side state (`servers`/`initialized`/`serving`/`closing`), and a
  goroutine dump.
- **`bd3a6cb`: `TestReleaseProcessedBehindCapacityBlockedRequest` first pass =
  nil.** Linux stress gave 0/700 failures. Candidate causes: (A) start-of-pass
  `cleanupResponses` frees capacity; (B) the release is processed while the
  low request is missing from the listing; (C) neither file is listed. macOS
  held-fd scan visibility is a hypothesis only. Discriminating diagnostics and
  a darwin `listNames` micro-test are proposed on the PR.

---

## PR206 combined-tree reassessment (with master #207/#209)

Posted as PR206 issuecomment-6407678613.
- **Root cause of the release-test failure is integration, not scan
  visibility.** #207 time-prefixes request IDs: 48-bit ms prefix followed by
  80 random bits. Branch-only tree: 0/700 failures. `3c67763` merged with
  master: 42/300 failures on Linux, because the release sorts ahead of the
  later "low" request unless both are signed in the same ms. My earlier
  all-zero premise was wrong because I read only the branch tree.
- **Pressure 39/40** happened before #207 (run created 03:16:14Z versus the
  #207 merge at 03:23:15Z), has not recurred in later macOS/Ubuntu runs, and
  remains unexplained. Classify the next occurrence with the diagnostics.
- **#207 × PR206 risk:** async hooks with `hookReportTimeout=10s` abandon
  requests. The abandoned request is still dispatched (the R2 shape), so its
  response is never released and pins a pool slot for 60s under a slow gate.
  Suggested a regression plus a signed client deadline.

---

## c0523b `a4c8e53` — history import, A5, launch fence (mailbox integration still pending)

Suites pass: accounts, codexapp, centralauth, claudecfg, codexcfg, panespec,
daemon, cmd/amux; `go vet ./...` is clean.

### A5 — SETTLED
The fresh-thread fallback (`isNoRollout`) is removed. Every resume target
(resumable, legacy v0, or PTY-pinned with no identity) fails the launch
visibly on any resume error. Re-running my A5 probe gives all three cases
`launchErr=true`, no `thread/start`, thread kept. Residual: a legacy
store-pinned thread whose rollout genuinely never existed now fails every
launch. Ask: make the error name a user-visible recovery (e.g. recreate the
workload), and document it.

### A1-R1 pending reuse — SETTLED
Every resolve gets its own 24-byte nonce (hash stored). Pinning a launch
retires all older pending launches; pending entries are capped at 16.
`ensurePane` (the only `engine.Ensure` caller, 6 sites) retires the launch
when the engine reused the live pane or the spawn failed, so attaches do not
accumulate. A killed process's late request can no longer claim its
replacement's permit. Minor: if the Lookup→Ensure race lets a concurrent
Ensure win, an unused pending launch is left behind until eviction. It is
harmless, because its nonce exists only in a discarded argv.

### History import (`codex_history_import.go`) — REPRODUCED gaps
Probe: `c0523b_history_import_probe_test.go.txt`, with a real PTY rollout in
the session's private `CODEX_HOME`.

| Case | imported | history in log |
|---|---|---|
| clean (control) | true; a second run imports nothing (idempotent) | yes |
| cold-start notice written first (`AppendNotice`, as `startStructuredForPrompt` → `structuredJournal("starting agent")` does before `ensureSupervisor`) | false | **no** |
| first import fails (rollout unreadable), a later append creates the log, cause repaired, retry | false | **no** |

Root's retry gap is confirmed, and **a second trigger needs no failure at
all**: any structured notice written before the import permanently disables
it. Today startup runs `importCodexHistories()` before restore and prompts,
so the trigger needs a failed or missed startup import (or a session that
gains a rollout later), and from then on every launch's own "starting
agent" notice keeps it skipped. Other notice writers:
`steer.go:347/366/378`.

Fix direction (no protocol change): treat "already imported" as an explicit
marker (e.g. a first-line import record, or a sidecar state), not mere log
existence. A log that contains only amux notices, or no import marker while
a rollout exists, is importable: merge by writing `import prefix + existing
notices` atomically, or keep notices in order after the prefix. On import
failure, do not proceed silently to `Ensure`: either fail visibly or record
retry-pending, so the supervisor's first append cannot end the retry window.

Other points:
- **Publication.** `os.Link(tmp, path)` followed by a deferred `Remove`
  leaves `Nlink=2` briefly, and readers that require `Nlink==1` may reject
  the file in that window. There is also no parent-directory fsync. Prefer
  the existing no-replace rename (`renameat2 RENAME_NOREPLACE` /
  `renamex_np RENAME_EXCL`), then fsync the directory.
- **Race.** If the supervisor's first append wins the link race,
  `EEXIST → (false, nil)` silently drops the import, which is the same
  permanent-skip class. Not reachable today, because import runs before
  `Ensure` in the same goroutine, but the comment claims safety for it.
- **Fidelity.** It uses the same cold PTY mapper
  (`runtimeevents.MapCodexLine`), so events match what the PTY reader showed,
  plus one meta notice marking the boundary. Pinned home and thread are
  correct (`Identity.ThreadID` else `ClaudeID`; harness path from `s.Dir`).
  There is no duplicate risk, because nothing backfills on resume.
- **Startup cost.** Startup imports synchronously for every row, reading
  whole rollouts before restore. That cost is one-time, but failed sessions
  retry on every startup. Bounded; note it in the docs.

---

## c0523b `1d88ab5` / `e502930` — importer v2 (marker, gated launch) and fixture seam

Probe: `c0523b_history_import_v2_probe_test.go.txt` (daemon package, overlay).

| Case | Result at `e502930` |
|---|---|
| (a) log rename succeeds, marker write fails (blocked `.history.json.tmp`), retry | **prefix imported twice** (`prefixCopies=2`) |
| (b) v0 identity with thread = rollout, no log | `imported=false`, marker `structured`, **no history**. Regression: `a4c8e53` imported this case using `Identity.ThreadID`. |
| (c) two concurrent first imports of one session | **34 of 50** trials have prefix count ≠ 1 (duplicated or lost) |
| (d) writer holding the log open across the rename (supervisor `logW` shape) | notice and history kept, **held writer's later line lost** (it goes to the orphaned inode) |

All four of root's concerns are confirmed. Actual daemon serialization:
- **Startup is safe.** `importCodexHistories()` (`daemon.go:443`) runs
  before the `ln.Accept()` loop and before restore, so no client or
  supervisor can race it.
- **After startup, (c) and (d) are reachable.** The import in
  `ensureSupervisorSpec` runs *outside* the manager's per-session Ensure gate.
  `paneOpen` (host connection, not under `effectMu`) and the steer prompt
  path (`admitDeferredEffect`) can enter it concurrently for the same
  never-structured session, e.g. two clients attach after a failed startup
  import. If caller A finishes, starts its supervisor (persistent O_APPEND
  `logW`) and caller B then renames over the log, A's whole runtime writes
  to an orphaned inode. The single post-rename `ReadAll` also ignores
  errors, and it cannot cover writers that keep the old fd.
- **(b) is decided by identity presence**, not by log content. A v0 or
  pre-event-log identity, or a log that holds only notices, gets
  `structured` with nothing imported.

Fix direction, without rewriting a live runtime's log:
- Run the import under the **same per-session gate** as `codexapp.Manager.Ensure`
  (or a per-session mutex shared by every `ensureSupervisorSpec` caller), and
  only when `d.codex.Get` is empty under that gate. No live `logW` can then
  exist.
- Make `AppendNotice` take a per-log lock (an flock on the log or marker)
  that the importer also holds across copy and rename. That removes the
  late-append race without `ReadAll`.
- Make retry idempotent: write an `importing` state (or embed a marker
  record as the log's first line, carrying the thread id) **before** the
  rename. A retry that finds the embedded prefix only finalizes the marker
  instead of importing again.
- Decide "structured" from log content: the log contains
  supervisor-originated events. Identity existence is not enough. A v0
  identity with no log, or a notices-only log, still imports from
  `Identity.ThreadID` (else `ClaudeID`), as `a4c8e53` did.
- Regressions: cases (a)–(d) above, each with a clean control.

### Fixture seam (`fixture_amuxfixture.go`, `//go:build amuxfixture`) — OK
- With no tags `FixtureSourcesEnabled()` is false; with `-tags amuxfixture`
  it is true.
- The release binary (`go build ./cmd/amux`) has **no**
  `accounts.init.0`; the tagged build has it. The Makefile `build` and `cross`
  targets pass no tags.
- In a release build a `FixtureOrigin` binding in `accounts.json` is inert,
  because `fixtureSource` returns "".
- CI uses `-tags amuxfixture` only for `TestAgentRuntimeCommandSurface`. The
  central-auth native tests (`TestSmokeCentralAPIKeyBindingNative`,
  `TestCentralClaudeLaunchDeliversTokenInsideProtectedNamespace`, the broker
  round-trips) run on the normal build.

---

## PR206 `e9781e2` → `fddfce0`

- **The macOS repeat reproduced the stall, and it is fixture-side.** At
  `e9781e2`, `pressure-2` recorded `launcher exit 126 on fetch 14` while
  `files=0` and every queue was empty. The temporary job reported success
  because its `| tee | grep` pipeline runs without pipefail.
  (issuecomment-6407840504)
- **`fddfce0` ordering fix is correct.** `ahead` is captured before the
  client signs, giving a strictly earlier ms prefix. macOS pressure: 15/15 at
  `fddfce0`, 29/30 overall.
- **Ubuntu failure** `TestAccountsPTYScrolling` is in nativetui, which PR206
  does not touch, and passes 20/20 locally on merged and master trees.
- **Remaining:** integrate current master (#212/#214/#204/#215), remove the
  temporary job, classify or accept the 1/30 launcher 126 (capture launcher
  stderr), and green CI on the final head.

---

## PR206 FINAL: signoff at `4d198e0`, conditional on green CI for that head

The production diff against master is five files, identical to the reviewed
`e3cdeea`. `10ddbb8` CI was green on all four checks. The merged-tree local
suites and pressure 5/5 passed. The docs record every accepted residual,
including the 1/30 exit 126 as unexplained, without overclaiming.

---

## c0523b `6017037` / `36662b6` — importer v3: root's three gaps FIXED

Probe: `c0523b_history_import_v3_probe_test.go.txt`, run through the real
`d.importHistory` with a real `codexapp.Manager`, under `-race`. No races.

| Case | v2 (`e502930`) | v3 (`36662b6`) |
|---|---|---|
| (a) marker sidecar write fails, then retry | prefix ×2 | **prefix ×1**, marker written (the in-log marker recovers) |
| (b) v0 identity, no log | `structured`, no history | **imported** via `Identity.ThreadID` |
| (b2) identity + notices-only log | — | imported, notice kept *after* the history |
| (c) 3 concurrent `importHistory` calls ×50 | 34/50 bad | **0/50 bad** |
| (d) 20 `AppendNotice` calls racing the import ×50 | held writer lost | **0 lost**, prefix ×1 |

Serialization and lock order (CODE-READ):
- `WithQuiescentLog` takes the per-session start gate, then `m.mu` (brief),
  then `LockEventLog`. It refuses with `ErrLogBusy` while a start is in
  progress or the process is running. The import is decided by log content
  (any non-notice event means `structured`).
- `AppendNotice` takes only `LockEventLog`, a leaf lock. Gate holders
  (`Ensure`, `Close`, `WithQuiescentLog`) never wait for `AppendNotice`. So
  the order is effectMu → gate → eventLog everywhere, with no cycle.
- The startup import also goes through `d.importHistory`, i.e. the gate.

Residuals (non-blocking):
1. **Exit-drain window.** The reaper closes `procDone` first, then
   `rpc.close()` and `<-rpcDone` (draining the final frames into `logW`),
   and only then `closeLog()`. In that window `processRunning()` is false,
   so a quiescent rewrite could orphan the last drained lines.
   Practically limited: a runtime that wrote any structured event makes the
   log `structured`, so it is never rewritten; only notices from a runtime
   that died before its first structured event are at risk. Cheap fix: treat
   the log as busy until the reaper has run `closeLog` (a `stopped`
   channel).
2. **Gate hold during import.** On the steer and host-action paths
   `ensureSupervisorSpec` runs under `effectMu`, so a large rollout's
   one-time import (a full read and rewrite) holds the daemon-wide gate for
   its duration, delaying credential delivery and reconcile. This is
   bounded (once per session, sized by the rollout), and the startup import
   runs before Accept. Document it, or measure it on a large rollout.

---

## PR213 pre-final review at `5845948` (PR206 merged as `7884a0e`; PR213 not yet integrated)

### Current fence after `5845948` removes the a3f9 committed-version check — ACCEPTED
- **Replacement paths audited.** The only writers of `CredentialVersion` are
  the bind/Accept bootstraps (when it is 0) and Claude login completion
  (`daemon/accounts.go:140-144`). Completion runs inside a login attempt whose
  start bumped `DeliveryEpoch` and set `login-pending` (`:252`), and it
  re-checks `p.DeliveryEpoch == a.epoch`. Logout bumps the epoch too. Nothing
  in `cmd/` writes the store directly.
- **Transactional fence (accounts-level probe,
  `c0523b_epoch_fence_probe_test.go.txt`).** A delivery that read its epoch
  and credential before the replacement gets `Accept` →
  `account_revision_changed` (ErrStale, retryable), and the retry pins the
  new version. The control shows the fence relies on every replacement
  bumping the epoch, which the audit above confirms.
- **At the real gate boundaries (`c0523b_replacement_race_stress_probe_test.go.txt`,
  `-race`, 3 runs).** Real `dispatchCentralCredential` (dispatchMu+effectMu →
  Delivery → Load → Accept) runs against 15 real `accountCall` login
  replacements: 9–10 launches with retries, **0 launches given two
  generations, 0 uncommitted generations**, retries after a replacement get
  `account_runtime_superseded`, and the final store and accounts versions
  agree. For Claude, delivery and replacement are also mutually exclusive
  under `effectMu`.
- **Reason for dropping the check holds.** With it, a partial login commit
  (store written, accounts.json not) would strand every new launch in
  ErrStale.

### BLOCKER for integration: PR206's pressure fixture vs PR213's launch fence
The trial merge with master `fa7bf75` has a textual conflict in
`central_launch_runtime_test.go` (imports only). After resolving it, PR206's
`fresh-launch-under-releasing-client-pressure` **fails on every background
session's first fetch**: `launcher exit 75`, stderr "this Claude pane started
without a credential launch". The fixture re-invokes the nested launcher
without `--amux-launch=`, and PR213 correctly refuses a launch-less delivery.
PR206's fail-fast and stderr capture named this immediately. The other five
subtests pass.

Resolution must keep PR206's assertions (240 releasing fetches, fresh first
attempt < LaunchRetry, pool peak bound, fail-fast and diagnostics). For
example, the test allocates one launch per background session with
`accounts.BeginLaunch` and hands `--amux-launch=<seq>:<nonce>` to the fixture
(via a file or env the test writes). Repeated fetches on one launch then get
the same snapshot, which is valid under the fence. Do not weaken the
launch-less refusal.

### Other items before final exact-head signoff
- **Document the one-time import gate hold** (root requires it). The import
  runs under `effectMu` on the steer and host-action paths, and a large
  rollout holds the global gate for its read and rewrite. Not yet in
  CHECKPOINT or machine-accounts.
- **Re-validate the CHECKPOINT gate statements against #216** (`fa7bf75`,
  "reconcile sessions without holding the global gate") once integrated.
- **Fixture seam:** still inert in release (tag-only setter, nothing outside
  tests writes `FixtureOrigin`).
- Exit-drain residual accepted by root as non-blocking.
