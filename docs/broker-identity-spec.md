# Machine login and broker identity: current behavior and proposed contract

Status: historical design baseline, merged in PR #172 (squash `59f4454`, final reviewed head
`6ef3fddedd4960a0df84d72326e00147bf68b5af`). The machine-account v1 implementation now adds
persisted profiles/defaults and workload bindings, protected login ceremonies, native/observational limits,
and account status/use transport. Configuration is now restricted to local amux CLI/TUI;
web accounts are read-only and legacy remote manage grants cannot enable mutation. See [implemented behavior and limitations](machine-accounts.md)
for operator setup, wire/API details, continuity and cancellation semantics. The evidence labels and pinned
source descriptions below still describe the original reviewed baseline; they are not blanket claims that
every proposed guarantee or optional feature is implemented. Source
baseline: `e6e4d4aaf9fcd3e962713a57441602503b61755b`. Source links are pinned to their reviewed revision. This
document is a mathematical and transition-table specification, not an executable model or a verification
claim. It follows the repository's Markdown design-document convention; discovery found no existing
formal-model language, checker configuration, or checker target.

Upstream delta reviewed through `b784ab5045b0aeff8fadc3fd1a9d14b8b0f57254`: coordinator creation/model
selection changed, and Codex now reconnects its control transport to a still-running supervisor/process and
exact thread, repeating token login before resume. A transport reconnect is therefore distinct from runtime
replacement; neither implies a new broker credential or account binding. The credential/policy/account-store
contracts below remain unchanged. See [manager lifecycle](https://github.com/Kchymet/agent-multiplexer/blob/b784ab5045b0aeff8fadc3fd1a9d14b8b0f57254/internal/codexapp/manager.go#L90)
and [reconnect handshake](https://github.com/Kchymet/agent-multiplexer/blob/b784ab5045b0aeff8fadc3fd1a9d14b8b0f57254/internal/codexapp/supervisor.go#L635).

Navigation: [current login inventory](#21-current-machine-login-inventory-e) · [state](#3-abstract-state) ·
[judgments](#4-authentication-and-authorization-judgments) ·
[transitions](#5-current-transitions-and-ordering-points) ·
[invariants](#7-candidate-invariants-and-their-status) · [desired
design](#9-proposed-desired-identity-contract-and-gaps) ·
[evidence](#11-sourcetest-evidence-appendix-and-next-review-step)

## 1. Scope and evidence labels

The central design requirement is machine-scoped Claude/Codex login configuration, account identity and
subscription-limit visibility through both amux TUI and web Harness, with every workload following the
machine's central configuration at its next process launch (§ P11, P17). The model connects this proposed account service to the existing access authority, session mailbox,
policy, credential lifecycle, and runtime fencing. External account services and effects are abstract
boundaries. Web Harness is a separate repository; its evidence comes from a source review at the pinned
revision in section 11. Cross-repository findings are distinguished from locally inspected code.

- **E — enforced behavior:** a check or ordering visible in the cited code.
  This is source evidence, not an exhaustive proof of every caller.
- **A — assumption:** a condition needed by the abstraction or a stated property.
- **P — proposed guarantee/design:** a review proposal; not a claim about what
  already ships. A proposal may preserve existing behavior while making its
  contract explicit.
- **Q — open question:** a choice or audit obligation not resolved here.

The existing prose contract is [agent CLI
lifecycle](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/docs/agent-cli-sandbox.md#availability-restart-and-completion). The [security
policy](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/SECURITY.md#intended-environment) trusts the operator, daemon, installed executable, selected
harness binaries, and OS. Sessions share network access and selected accounts; macOS host application
launching is a deliberate exception. This is not a hostile-tenant isolation specification.

## 2. Entities and disjoint identity namespaces

Treat the following sets as disjoint, even where their wire representations are all strings. Equality across
sets is a type error in the model.

| Entity / notation | Current meaning and lifetime | Evidence |
| --- | --- | --- |
| Subject `s = (kind, subjectID)` | `kind` is host, session, or provider. The local host subject is `local-operator`. A session subject identifies a store row or the synthetic console. | [authority.go:42–81,169](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L42), [session_policy.go:125](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_policy.go#L125) |
| Principal `p = (s, k, g)` | Authenticated subject plus key ID `k` and **credential Generation** `g`, a positive integer. Role and scope are deliberately absent. | [authority.go:62–101](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L62) |
| Credential key `k` | Ed25519 possession credential; registry contains public material and validity, client file contains private material and daemon trust pins. Normally valid for 30 days. | [authority.go:600–616](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L600) |
| Daemon boot `b` | **BootID**, a fresh 32-byte random nonce on authority open. Registry, issuer, TLS identity, and replay ledger are loaded separately and can persist. Boot is not credential generation. | [authority.go:224–259](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L224) |
| Conversation `c` | Persisted harness conversation identity (`ClaudeID` historically names the field, including Codex usage). A runtime restart can preserve it. It is not broker authority. | [store.go:30–40,325](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/store/store.go#L30), [actions.go:377](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/actions.go#L377) |
| Runtime `(h, rho)` | Process-local handle identity `h` and opaque **runtime generation** `rho` (16 random bytes). Re-observing the same handle retains its generation; replacement or retirement changes the binding. | [session_permission.go:77–146](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_permission.go#L77) |
| RPC request `q` | **RequestID**, normally 16 random bytes per signed envelope. Replay key is `(k,g,q)`; BootID and body digest are signed but are not components of that ledger key. | [authority.go:964–980](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L964), [authority.go:1077–1112](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L1077) |
| Permission request `a` | Runtime approval ID, bound to `(session,rho,h)`. It is a separate namespace from RPC `q`, even though both use request-ID terminology. | [session_permission.go:222–286](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_permission.go#L222) |
| Receipt | A new signed RPC envelope with its own `q`, containing the original RPC ID and response digest. | [client.go:280–310](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/client.go#L280) |
| Account token `t` | An upstream account capability, separate from the broker signing credential and its Generation. Central credential delivery also has its own account-store generation. | [central_credential.go:76–87](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_credential.go#L76) |
| Machine `m` (P) | Stable broker installation/owner identity, distinct from BootID and from the physical device's other OS users. Remote clients must bind to this authenticated machine, not choose authority by supplying a machine ID. | Proposed scope; current homes derive from local amux data directories. |
| Profile `f`, account `u`, credential version `v` (P) | `f` is a machine-local configuration identity. `u=(provider,providerAccountID,workspace/tenant)` is verified upstream identity, or explicitly unknown; email is display data. `v` identifies a secret revision within a profile. None equals broker Generation, BootID, runtime generation, or another profile's version. | Proposed distinction; current central Claude and Codex token generations are unrelated counters. |

**E:** The session is the credential trust unit. Every tab/process able to read its credential can sign as
that session. There is no authenticated model, pane, tool, or individual process identity inside it. Fixed
context and environment locators do not add authority. A stable read-only credential directory permits running
mounts to see rotated `current` files. Thus rotation alone does not exclude a still-running process that can
read the successor key. Sources: [central_credential.go:17–21](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_credential.go#L17),
[paths.go:25–47](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/paths.go#L25), [authority.go:83–86](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L83).

### 2.1 Current machine login inventory (E)

| Concern | Implemented behavior and boundary | Evidence |
| --- | --- | --- |
| Defaults and sources | Central is the default per harness. Claude supports central/host/shared/environment; Codex central/host/environment. These select local stores/config, not persisted workload profile IDs. Host/shared expose legacy shared login semantics; environment supplies no amux credential. | [amuxcfg/auth.go:10–44,91](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/amuxcfg/auth.go#L10), [store.go:30](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/store/store.go#L30) |
| Claude acquisition | Host CLI runs setup-token and hidden paste or token-stdin, saves one private central token, and queues idle-agent restart. Status reports fingerprint, stored time and credential generation, not verified account ID/plan/expiry. Setup-token's reported one-year lifetime is not a parsed expiry. | [cmd/amux/auth.go:116–213](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/cmd/amux/auth.go#L116) |
| Claude delivery/overrides | Agent launcher fetches via signed mailbox; every credential-reading session process has that authority. Central conflicts with API-key/helper/provider/endpoint overrides fail closed. Host/shared use legacy file/Keychain sources; central/environment retire shared login links. Reissue does not revoke old tokens; busy agents retain them until restart. | [central_credential.go:17–93](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_credential.go#L17), [claudecfg/central.go:30–55,155](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/claudecfg/central.go#L30), [auth.go:57–101](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/auth.go#L57) |
| Codex acquisition/status | Daemon owns central home/native App Server; host action starts/cancels login, host query returns mode, email, plan, state and URL. Central mode routes supervised sessions to this owner; changing into that mode can require daemon restart. | [central_codex.go:51–75,132–214](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_codex.go#L51), [core.go:257–281](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/core/core.go#L257) |
| Codex delivery/continuity | Supervisor callback uses daemon-supplied session ID; shim checks existing unarchived Codex row, **not broker Valid or credential generation**, and does not recheck after provider I/O. Refreshes coalesce; nonempty previous-account binding rejects account changes. Binding persists in the structured identity sidecar, best-effort, and is restored for the matching resumed thread; it is not a creation-time profile record. | [central_codex.go:100–129](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_codex.go#L100), [codexauth.go:246–281](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexauth/codexauth.go#L246), [manager.go:311](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexapp/manager.go#L311), [identity.go:67–100](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexapp/identity.go#L67) |
| Codex overrides/logout | Central strips selected API-key/endpoint variables, does not share host auth.json, and injects access tokens before thread use. Host mode shares host login. Provider has native Logout, but amux auth CLI exposes login/status/restart, not logout. Closing provider is not upstream logout. | [codexcfg/template.go:42–59](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexcfg/template.go#L42), [codexapp/auth.go:184–205](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexapp/auth.go#L184), [codexauth.go:471–509](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexauth/codexauth.go#L471), [cmd/amux/auth.go:35–39](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/cmd/amux/auth.go#L35) |
| UI/web and limits | No account-management TUI integration, account quota model, or account/rateLimits consumer found in this checkout. Claude statusline subset takes model/session only; usage events measure tokens/context, not remaining subscription. Provider creation accepts machine identity, meaning execution-machine config, and its advertisement intentionally contains no account IDs. See the independent Harness audit in section 11. | [claudecfg.go:643–651](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/claudecfg/claudecfg.go#L643), [codex.go:511–524](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/runtimeevents/codex.go#L511), [execution.go:3–19](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/harnessproto/execution.go#L3), [sessions.go:348–378](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/provider/sessions.go#L348) |

**Documented adapter possibilities, not implemented guarantees:** Codex official docs expose account/read,
login start/cancel/completed, logout, account updates, and rateLimits read/updated; external-token mode
requires host-managed refresh. Claude documents auth login/logout/status JSON and statusline rate_limits with
optional percentage/reset windows, available only for supported account types after an API response. Missing
data is not unlimited. These docs do not establish that a central opaque Claude setup-token reveals verified
account identity. Syntax validation is not issuer authentication; host Claude auth status may describe a
different login. Native logout does not delete amux's separate setup-token store. The Codex adapter expects a
browser authUrl, not a device-code result. Installed CLI version support was not verified; smoke quota calls
are not proof of decoding support. Workgroup readiness must check each selected harness, including a Codex
coordinator plus Claude workers. Sources checked
2026-10-24: [Codex account APIs](https://learn.chatgpt.com/docs/app-server#auth-endpoints), [Claude
CLI](https://code.claude.com/docs/en/cli-reference#cli-commands), [Claude
statusline](https://code.claude.com/docs/en/statusline#available-data).

## 3. Abstract state

Let `None` denote absence and `map[X -> Y]` a partial map. A model state is:

| State | Meaning / persistence |
| --- | --- |
| `owner, b, now` | Authority ownership/open flag, current boot, clock. Ownership is protected by an exclusive file lock; clock is an external input. |
| `Reg[k]` | `(s,g,publicKey,issuedAt,notAfter,revokedAt)` credential records. Persisted in `registry.json`. |
| `Cur[s]` | Current key or `None`, in the same registry file. Historical records remain after revocation. |
| `Stage[s,k], Pub[s]` | Staged private credential and published `current` file. Separate publication from registry authority. |
| `Seen[k,g,q]` | Accepted request digest and expiry, persisted in `replay.json`; bounded and pruned. |
| `Catalog[s]` | Current role, parent/root, scope, repos, archive flag, harness, conversation, directory. SQLite-backed except synthetic projections. |
| `Grant[s] = (initialized,repos)` | Persistent coordinator repository ceiling. Initialized-empty differs from unknown/uninitialized. |
| `Run[s] = (h,rho,observed,claimed,excluded)` | Volatile runtime binding and permission lifecycle sets. |
| `RPC[q]` | Ghost state for reasoning: published, claimed, accepted, denied, dispatched, response-visible, response-synced, observed. Not a new production journal. |
| `Work[q]` | Abstract external/store effects and queued work. An operation can have zero, one, or multiple subeffects. |
| `Completion[s]` | Volatile owner `(p,q,h,rho,digest,phase)` of self-completion cleanup. |
| `Disclosed[s]` | Ghost set of account tokens/data already delivered; monotone, because amux cannot retract copies. |
| `Profile[m,f]` (P) | Harness, source mode, account identity/evidence, secret reference, credential version `v`, revision, delivery epoch, and state (unconfigured/login-pending/ready/blocked/logged-out). Secret references are internal, never UI fields. |
| `Default[m,harness]` (P) | At most one selected local profile and default revision per harness; absence is explicit, never fallback to another login. |
| `Bind[w]` (P) | Persisted workload `(m,harness,f,expectedAccount,bindingRevision,selectionOrigin,state)`; origin is creation-time default resolution or authorized override. Resolve each runnable workgroup member/coordinator separately for its harness. |
| `LoginAttempt[m,f,l]` (P) | Initiator, expected profile revision, expiring attempt ID, provider flow correlation, state; separate from RPC and permission IDs. Secrets stay in a private staging store until commit. |
| `Quota[m,f,u,bucket]` (P) | Optional measurements, observedAt, receivedAt, source, freshness/unavailable reason, attribution confidence, profile revision and binding/runtime provenance where relevant. |
| `MachineRights[actor,m]` (P) | Separately granted read-account-status and manage-login; neither follows merely from session membership, publication, compute access, or a request's machine ID. |
| `ProfileUse[actor,m,f]` (P) | Explicit use-profile grant to an authenticated actor for a particular machine/profile. Default use may instead derive from existing authorized workload creation, as defined in section 9.1; it grants no account read/manage rights. |

For persisted files, distinguish **visible** state from **sync-confirmed** state. The implementation's
`committed` flag means rename has made the pathname visible; directory sync can subsequently fail. It does not
mean confirmed power-loss durability. On such an error the authority still adopts visible registry/replay
state in memory. [authority.go:1186–1222](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L1186)

**A:** One authority owner; unforgeable signatures; collision-free fresh keys and nonces within the bounded
run; no hostile alteration, deletion, or rollback of daemon storage; functioning OS isolation of other
subjects' credentials. A session may retain/copy its own credentials and produce arbitrary signed calls.

**A:** For the initial crash model, process restart reloads the current filesystem view. Power loss is
separate: unsynced renames may recover old or new state, subject to filesystem ordering; confirmed syncs
persist. This is a conservative abstraction, not a specification of every filesystem. SQLite transaction
durability is an explicit environmental assumption; there is no transaction joining SQLite, registry, replay,
and response files.

**A:** Time progresses without rollback for the initial expiry proof. The code uses wall time; clock rollback
and deliberate request-ID reuse require additional analysis rather than an unconditional lifetime
deduplication claim.

## 4. Authentication and authorization judgments

Define the authority validity judgment, for `p=(s,k,g)`:

    Valid(p, S) iff
      owner = open
      and Reg[k] exists and Reg[k].subject = s and Reg[k].generation = g
      and Cur[s] = k and Reg[k].revokedAt = 0
      and now < Reg[k].notAfter

This describes [Valid](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L1003). It authenticates a current credential; it says
nothing about whether the subject is archived or allowed an operation.

For envelope `e=(version,b,k,g,q,issued,expires,body,signature)`, define:

    EnvelopeOK(e, S) iff
      version = 1 and e.boot = b and q has the required shape
      and 0 < len(body) <= 64 KiB
      and expires >= now - 5 seconds and issued <= now + 5 seconds
      and issued <= expires and expires - issued <= 30 seconds
      and Reg[e.k] exists
      and Valid((Reg[e.k].subject, e.k, e.g), S)
      and signature verifies over the domain-separated framing of
          boot, key, generation, q, issued, expires, SHA256(body)

`Accept(e)` additionally requires `(k,g,q) not in Seen`, available capacity after expiry pruning, and
successful replay persistence. Verification derives the subject from the registry, not caller fields. The
mailbox then requires `p.kind=session` and `p.subjectID=mailbox.subjectID`, decodes the exact accepted body,
and performs authorization. A cryptographically accepted request can therefore consume its ID before being
rejected for wrong mailbox, malformed operation, or policy.
[authority.go:926–992](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L926),
[server.go:395–458](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/server.go#L395)

For the host stream, pinned daemon TLS and an issuer-signed challenge establish server identity; a signed
connection-specific proof establishes the client principal. Only host kind is admitted by this listener. This
is a different protocol from mailbox `Seen`; one proof per connection consumes the challenge. Checks/watchers
invalidate connections as credentials change, but are not a proof that an already-started socket write can be
recalled. [socket.go:20–59,150–189](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/socket.go#L20),
[daemon.go:786–826](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/daemon.go#L786),
[authority.go:1029](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L1029), [conn.go:24–30](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/conn.go#L24)

Define `Allowed(p,op,S)` separately from `Valid`. For ordinary session RPC:

    SessionAllowed(p, op, S) iff
      Catalog[p.subject] exists and is not archived
      and CanonicalOperation(op) succeeds
      and RoleRule(Catalog, Grant, p.subject, op)

The actual admission judgment is `Valid AND SessionAllowed`. No valid signature can bypass canonical request
validation. Effective permission is the intersection of policy and dispatcher rules: for example, the policy
helper lists some unarchive cases, but restricted canonical operations reject restore.
[session_dispatch.go:19–48](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch.go#L19),
[session_operation.go:13–36,128–142](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_operation.go#L13)

| Subject / operation | Effective current rule (E) |
| --- | --- |
| Host | Broad host control after authentication; individual handlers still validate operations. `Policy.Authorize` returns true for host kind. |
| Ordinary session | Self rename and explicit self archive; own normalized history; limited queries and separately validated self reports/credentials. |
| Coordinator | Selected actions on self/direct members; child creation under its own root with explicit requested repositories within its persisted ceiling. Self-completion is separately constrained. |
| Repo home | Its own management context only; may create workgroups explicitly limited to its repo, but sharing a repo grants no control over another workgroup's members. |
| Console | Broad enumerated control actions, still subject to canonical restrictions and specialized handlers. |
| Discovery | Any active authenticated session may enumerate conversation metadata through `agent-sessions`; returned paths grant no filesystem access. |
| History | Ordinary self; coordinator current direct members (including archived members); repo home its owned descendants; console broader visibility. Caller archive and target archive have different meanings. |
| Provider | Declared kind; policy delegates to an injected resolver. Missing callback denies. No production assignment of that callback was found in this baseline; do not equate remote bearer capabilities with this local principal. |

Sources: [policy.go:47–245,303](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/policy.go#L47),
[session_policy.go:88–140](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_policy.go#L88),
[agent-cli-sandbox.md:48–81](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/docs/agent-cli-sandbox.md#authority-and-discovery). Roles themselves are derived from
store fields, not signed role assertions: [migrate.go:205–249](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/store/migrate.go#L205).

Specialized judgments bypass the general action vocabulary but not authentication:

- `CredentialAllowed`: own session, no target/tab, permitted operation/account,
  existing unarchived row, and matching harness where required. GitHub reads and
  Claude host-store operations have distinct allowlists. Central Claude delivery
  additionally checks configured mode and launch conflicts.
  [credential_broker.go:16–80](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/credential_broker.go#L16),
  [central_credential.go:29–93](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_credential.go#L29)
- `ReportAllowed`: own live session; runtime generation must match for a report.
  The current generation is queryable using the session credential. It fences
  stale messages, not competing processes with the same session authority.
  Hook permission observations are diagnostic, not answerable approvals.
  [session_reports.go:229–280](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_reports.go#L229),
  [agent-cli-sandbox.md:35](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/docs/agent-cli-sandbox.md#dispatcher-inventory)
- `ReceiptAllowed`: valid exact completion principal, original request and
  response digest, live pending receipt/grace state. It is the narrow exception
  to ordinary denial after the caller is archived, not a new action capability.
  [session_dispatch.go:23–30](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch.go#L23),
  [session_lifecycle.go:458–463](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_lifecycle.go#L458),
  [server.go:626](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/server.go#L626)

## 5. Current transitions and ordering points

Rows describe successful paths unless failure is stated. A logical operation can span several rows: **no
single atomic transition is assumed across all rows**. “LP” below means an identified local
linearization/ordering point, not a proof of whole-system linearizability.

| Transition | Preconditions | State change / ordering point | Evidence |
| --- | --- | --- | --- |
| Open authority | Exclusive root ownership; persisted state readable | Load registry/replay/issuer/TLS; generate fresh `b`. Second owner fails. | [authority.go:189–259](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L189) |
| Ensure first credential | Subject has never had a registry record | Stage generation 1, then commit and publish below. Existing revoked/expired subjects are not implicitly issued by Ensure. | [authority.go:348–379](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L348) |
| Stage successor | Rotate: valid current; Regrant: no current and expected latest revoked generation; RenewExpired: exact current key, unrevoked, elapsed lifetime | Create fresh key at predecessor generation + 1; write/sync staged credential and directory ancestry. No authority yet; pre-registry failure leaves predecessor authority unchanged. | [authority.go:433–503,561–639](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L433) |
| Commit registry | Staged credential available | Registry rename installs new record/current and marks predecessor revoked in one file; adopt in memory and notify old-key watchers even if later sync fails. **LP: registry rename**, serialized by authority mutex; sync is durability confirmation. | [authority.go:641–653](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L641), [authority.go:1186](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L1186) |
| Publish credential | Registry points to staged successor | Rename stage to stable `current`, then sync directory. **Visibility point: credential rename.** `Current`/`Ensure` can recover committed staged publication; they do not mint another generation. | [authority.go:382–403,654–660](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L382) |
| Revoke / RevokeCurrent | Revoke targets current subject; exact variant additionally requires matching `(s,k,g)` | Registry removes current mapping and marks record revoked. **LP: registry rename.** No-current ordinary revoke is a no-op; stale exact revoke fails. Not a store-archive or upstream-token operation. | [authority.go:873–923](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L873) |
| Publish/claim RPC | Signed envelope, bounded files/queue | No-replace request rename makes request visible; server claims it and reserves response capacity. Claim alone does not authenticate or authorize it. | [files.go:174–187](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/files.go#L174), [server.go:361–411](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/server.go#L361) |
| Accept RPC | `EnvelopeOK`, replay key absent, capacity | Persist `Seen[k,g,q]` before returning principal. **LP: replay rename**; dispatch proceeds only if Verify returns success. An uncertain commit consumes in memory but returns error. | [authority.go:964–992](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority.go#L964) |
| Admit ordinary action | Valid, canonical, authorized under dispatch/effect gates | Recheck live authority and policy, then call exact action. Deferred work carries a guard and reacquires admission before its bounded effect/queueing. **Ordering point is path-specific admission**, not eventual external completion. | [session_dispatch.go:82–155](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch.go#L82), [steer.go:442–465](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/steer.go#L442) |
| Read/disclose | Initial authorization; bounded read; final authorization | Discovery/history recheck after I/O; credential operations recheck within their gate. Return body to transport, which subsequently publishes it. Final check and response publication are distinct points. | [session_discovery.go:68–81](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_discovery.go#L68), [session_events.go:155–172](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_events.go#L155), [credential_broker.go:49–80](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/credential_broker.go#L49) |
| Publish response | Dispatch returned a bounded result | Sign exact result and request correlation; no-replace rename, then directory sync. May become visible despite reported sync failure. No general authorization callback at this publication point. | [server.go:481–548](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/server.go#L481) |
| Observe response | Signature, boot, subject, key, generation, request ID, body digest match original request | Client accepts result; success can precede receipt. An existing matching response is checked before detecting a newer service boot. A lost result is indeterminate, not automatic re-execution. | [client.go:175–196,230–310](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/client.go#L175) |
| Set grants / move | Host-authorized grant change, or permitted management action; store validation | Grant ceiling replacement commits in one SQLite transaction. Move changes stored parent without moving worktree. Neither implies credential rotation or retroactive filesystem revocation. | [grants.go:40–82](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/store/grants.go#L40), [wsops.go:459–514](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/wsops/wsops.go#L459) |
| Delegate child creation | Root valid; requested repos satisfy coordinator ceiling; canonical restricted call | Daemon creates distinct child ID/row/worktree; launch provisions its own key. No signed chain of attenuated credentials is passed from coordinator to child. Creation includes multiple non-atomic filesystem/store steps. | [policy.go:127–143](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/policy.go#L127), [wsops.go:190–235](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/wsops/wsops.go#L190), [session_access.go:65–81](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_access.go#L65) |
| Publish runtime | Valid launch path; runtime creation succeeds | Under runtime gate: capture historical pending-request exclusions, create handle, install fresh `rho` when handle changes. Same handle preserves `rho`. Partial creation errors are not modeled as successful publication. | [session_permission.go:77–142](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_permission.go#L77) |
| Bind / consume permission | Matching live handle; request observed/open, not historical or consumed; consumption also matches `rho` | Bind adds observed ID. Consume inserts into `claimed` under gate **before** final validation/delivery; even delivery failure cannot safely retry the same approval. | [session_permission.go:222–286](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_permission.go#L222) |
| Restart runtime | Host-authorized, active session, launch preflight | Replace agent/App Server, preserve conversation and ordinary sibling tabs. Runtime generation is retired/replaced. This is not credential rotation or a new daemon boot. | [actions.go:377–450](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/actions.go#L377) |

### 5.1 Limits of atomicity

**E:** Ordinary dispatch, host `handle`, reconciliation/renewal, credential operations, and completion cleanup
participate in daemon gates. Deferred steering reacquires them. This demonstrates ordering among those
participating paths. It does not establish that every store writer, every call to authority methods, or every
effect in another process participates. Policy lookups can use separate read-only store opens; there is no
universal policy snapshot transaction. [actions.go:31–49](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/actions.go#L31),
[session_lifecycle.go:136–140,534](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_lifecycle.go#L136),
[session_policy.go:54–115](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_policy.go#L54)

**E:** Authorization checks sample validity/expiry. Time passing is not blocked by a mutex. Input queueing may
happen under admission while consumption and a model turn happen later. A successful steering response can
mean accepted work, not an applied downstream result. Revocation cannot roll back those external effects.
[steer.go:33–50,155–177](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/steer.go#L33)

**E:** General response publication occurs after `Dispatch` returns and its deferred unlocks run. Final read
authorization therefore does not imply `Valid/Allowed at response rename` or `Valid/Allowed at client
observation`. Existing response bytes can outlive a daemon stop; delivered account tokens cannot be revoked by
amux. [server.go:481–503](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/server.go#L481), [SECURITY.md:31–45](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/SECURITY.md#L31)

## 6. Archive, completion, restore, and restart semantics

Archive state, key revocation, and runtime death are three separate facts.

| Lifecycle step | Preconditions / effect (E) |
| --- | --- |
| Self-completion | Exact self-completion shape and current runtime checks; commit archival before successful completion response. If commit outcome is uncertain but archive is true/unknown, establish bounded cleanup and return indeterminate. |
| Receipt window | Keep original principal/runtime ownership and response digest. Archived policy denies ordinary calls; narrowly matching receipt remains eligible while key is valid and receipt state permits it. |
| Finish completion | After receipt/abandonment and runtime grace, take effect gate; require same completion owner and archived row. Revoke exact completion principal; on revoke error return without killing here. Then stop only matching runtime token. |
| Reconcile after restart | Volatile completion entry is gone. Archived rows trigger server closure, revoke attempt, and runtime cleanup. **This path logs a revoke error and still calls killRuntimeFor**; unlike completion finish, observed runtime death does not prove successful revocation. |
| Host restore | Drain old completion before effect admission; retain/repair archived state; retire runtime binding and quiesce tracked tabs/App Server; regrant expected latest revoked generation when needed; confirm published successor before unarchive. Recover partial committed publication by inspection, not blind repeated issuance. |
| Failed restore | Archived/current and archived/revoked states are explicit recovery states. If unarchive failed, attempt to retire an unused successor; archived policy remains the protection if that retirement also fails. |
| Expiry maintenance | Active unrevoked credentials rotate near expiry; lapsed exact current keys can be renewed explicitly. Archived, revoked, or superseded keys cannot take that renewal path. Ensure alone does not renew. |
| Daemon restart | Reopen persistent authority with a new BootID; old envelopes fail boot binding. Old matching responses may still establish earlier success. Accepted work without a response is indeterminate. Runtime permission bindings are re-established; historical approvals must not inherit a replacement generation. |

Evidence: [session_dispatch.go:110–136](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch.go#L110),
[session_lifecycle.go:407–463,505–550](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_lifecycle.go#L407),
[session_lifecycle.go:351–359](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_lifecycle.go#L351),
[actions.go:31–47,174–313](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/actions.go#L174),
[session_lifecycle.go:284–332](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_lifecycle.go#L284),
[client.go:230–310](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/client.go#L230).

**A/Q:** Restore verifies tracked engine handles and supervisor state. Treating that as quiescence of *all*
credential-reading descendants requires a process containment/lifecycle assumption not proven by this broker
review. This matters because the stable credential mount exposes successors to survivors. Upstream tokens
already copied remain outside restore's revocation power.

## 7. Candidate invariants and their status

Use `Always(P)` for a safety property over traces. These are review targets; none has been mechanically
checked here.

| ID | Property | Status and qualification |
| --- | --- | --- |
| I1 | `Valid(p) => Cur[p.s]=p.k` and record subject/generation match | **E**, direct validity checks, assuming intact authority state. |
| I2 | At most one current credential per subject; each successful successor has `g_new=g_old+1` | **E**, map update and successor validation under authority lock. Random key collision freedom is **A**. |
| I3 | No Ensure/launch/reconcile silently resurrects an explicitly revoked key | **E**, historical records plus explicit Regrant; assumes registry is retained, not deleted/rolled back. Expiry-only renewal is distinct. |
| I4 | Same replay identity cannot be accepted twice while its ledger entry is retained | **E**, persistence before principal return. This is **at-most-once admission**, not exactly-once effects or successful dispatch. |
| I5 | An exact old envelope cannot become valid again after replay pruning | Requires nondecreasing time/fresh BootIDs (**A**). Entries expire; a signer can later reuse an ID in a newly signed envelope. Lifetime ID uniqueness is an additional **A/P**, not enforced forever by the ledger. |
| I6 | Restricted effect admission uses authenticated subject and canonical operation, with current policy checks | **E** on cited guarded paths. Extending it to every action/subeffect/writer requires a complete gate audit (**Q**). |
| I7 | Consumed approval cannot be delivered again to a replacement runtime under the old `(h,rho,a)` | **E**, exact runtime matching and claimed set. No implication that the approval originates from a unique process within the session. |
| I8 | Stale completion cannot revoke a successor credential or kill a different runtime token | **E** in exact completion path, owner checks, RevokeCurrent, and killRuntimeToken. Generic reconcile is a distinct transition. |
| I9 | `Disclosed[s]` never shrinks as a result of local revocation | **A** modeling knowledge/copies; reflects explicit security limitation. |
| I10 | Creation requests accepted from a coordinator stay within its current initialized repository ceiling | **E** for canonical explicit repo requests plus execution-time checks. Not an invariant that all existing child mounts remain a subset after ceiling changes or moves. |
| P11 | Every `Bind[w]` follows the machine's central profile for its harness; a committed replacement (account, mode or credential version) is delivered only to a launch `L` of `w` whose first delivery follows the commit | **E**, account store v2 (`accounts.AcceptAuthorized`), regression tests in `internal/accounts` and `internal/daemon`. Legacy host/shared/environment sources are retired by a one-time migration; no credential is copied. |
| P17 | For one launch `L` of `w`: deliveries after the first return the pinned snapshot `(mode, account, version)` or are refused (`account_runtime_superseded`); a delivery for `L' < L`, an unallocated `L`, or a wrong nonce is refused; pinning `L` retires every pending `L' < L`. A resumed Codex thread keeps its exact thread id under the new account; a resume error fails the launch and never substitutes an empty thread | **E**, launch sequence plus per-launch nonce (hash only persisted), daemon-side allocation (`BeginLaunch`, `ensurePane`); pinned Codex 0.157.1 synthetic fixture for cross-account resume. A runtime-consistency fence, not isolation inside a session (all tabs share its principal). Backend acceptance of another account's encrypted reasoning is not provable with fixtures (**A**). |
| P12 | Same-account refresh changes credential version, not verified provider/workspace identity | **P**, stable provider IDs where available; email is display metadata. Unknown identity remains unverified and cannot certify account continuity. |
| P13 | A login commit matches `(machine,profile,initiating actor,attempt,expected revision/epoch)` and current manage authority | **P**, cancelled/stale callbacks and offline queued operations cannot overwrite newer selection, logout, or replacement. |
| P14 | Disabled/logout profile cannot admit new delivery; stale credential acquisition cannot publish past its disabled epoch | **P**, affects future issuance and workload readiness, not immediate external invalidation of already-issued tokens. |
| P15 | Quota snapshots match account/profile revision and launch binding/runtime generation of their source | **P**, source/trust/freshness mandatory; old-account reports discarded, session reports advisory/forgeable, unknown never unlimited, workload percentages never summed. |
| P16 | Account secrets are absent from UI, read/control API output, logs, and durable relay payloads | **P**, allowlisted projections and private acquisition/delivery channels; approved ephemeral login navigation is scoped to its initiating manager. |

Useful counter-properties to reject: “every accepted request has one effect,” “every error means no effect,”
“runtime death proves key revocation,” “credential rotation proves old-process exclusion,” and “no response
can arrive after revoke.” None follows from the current implementation.

## 8. Worked traces

These traces illustrate allowed outcomes and candidate counterexamples. They are derived from source ordering;
they were not executed as tests during this review.

| Trace | Steps | Consequence |
| --- | --- | --- |
| Duplicate admission | Accept `(k,4,q)` and sync Seen; publish duplicate envelope | Duplicate is refused even if the first operation was denied or produced no effect. Same body with a fresh request ID is a new operation, not deduplicated by body. |
| Crash after acceptance | Sync Seen; crash before Dispatch; reopen with `b2` | Effect may never occur. Original boot envelope is rejected; client must not manufacture a fresh request to retry an indeterminate mutation automatically. |
| Lost result | Accept; mutate store or queue external work; crash before response | Effect may have happened. Replay prevention does not reconstruct the result or compensate the effect. |
| Partial rotation | Start with `Cur[s]=k4, Pub[s]=k4`; stage k5; registry rename/sync; fail current-file rename | `Valid(k4)=false`; registry points to k5 while client still reads k4. Current can publish staged k5 without issuing k6. Failure before registry rename instead leaves k4 authoritative. |
| Uncertain durability | Registry/replay rename succeeds; directory sync fails | API returns error, but live authority adopts new state. Process-only reopen observes current files; power-loss recovery needs the separate persistence assumption. Do not classify this as “nothing committed.” |
| Revoke after read check | Final authorized read check passes; gate released; revoke commits; transport publishes prepared response | Not excluded by the generic response-publication code. A stronger publish-time revocation contract would require an additional boundary. |
| Completion versus restore | Archive under k4; response/cleanup pending; host drains old cleanup, quiesces tracked runtimes, regrants k5, unarchives | Delayed k4 cleanup cannot revoke k5; exact runtime token cannot kill its replacement. The operation spans several recoverable states, not one transaction. |
| Runtime replacement | Bind approval a to `(h1,rho1)`; replace with `(h2,rho2)`; submit decision for rho1 | Denied. A process holding the session credential can still query rho2 for future self reports; rho is not process authentication. |
| Grant shrink | Child already has repo R; host replaces coordinator ceiling with empty initialized set | Future explicit child creation with R is denied. Existing child/worktree/account-token revocation is not implied by this grant-table update. |
| Default switch (P) | Create w1 under fA; set default fB; create w2; restart w1 | w1 retains fA/account A and w2 records fB/account B. Restart alone cannot silently select B. |
| Late login/logout (P) | Start login l at revision 2 for actor X; logout advances disabled epoch; callback l arrives | Callback cannot enable delivery or overwrite selection. External cleanup may be pending; existing copied token validity remains an upstream fact. |
| Stale quota (P) | Session reports A/rho1; explicit rebind to B/rho2; delayed report for A arrives | Reject attribution to B. Offline machine or unsupported adapter displays stale/unavailable, never zero usage or unlimited capacity. |

## 9. Proposed desired identity contract and gaps

### 9.1 Machine profiles, workload binding, and account lifecycle (P)

TUI and web Harness use the **same machine broker service**. Its owner domain is `(broker installation, OS
user, config/data domain)`, not a provider registration, browser session, or remote user account. Multiple
provider registrations mapping to that domain share one owner and one default per harness; they cannot start
independent refresh writers. Stable domain identity needs authenticated binding and clone/migration handling;
BootID cannot substitute for it.

Each harness has at most one default profile, while other retained profiles may support explicit overrides and
old workload bindings. A profile is a stable local configuration ID, not email, upstream account ID, or
credential version. Ready central profiles bind an immutable verified account where available. Account
replacement creates a new profile (or a new immutable account binding) rather than masquerading as
same-account refresh. Unknown Claude identity remains unknown: a changed opaque token cannot be certified as
same-account refresh.

`use-profile` authorizes binding a workload to an account source, not reading its secrets or managing login.
Its principal is the authenticated initiating actor (local operator or explicitly mapped remote actor), and
its resource is `(machine, profile)`, constrained by existing workload-create/management authority. Proposed
default policy: an actor already authorized to create workloads on a machine may use that harness's current
machine default without manage-login. Selecting an alternate profile requires an explicit `ProfileUse` grant;
manage-login alone does not imply use-profile. Default resolution and this check occur together at creation.
The recorded binding then authorizes delivery to that workload's fenced session/runtime; creation does not
transfer the actor's general permissions to the session. Explicit rebind requires workload management,
manage-login, and an explicit target-profile use grant, even when the target is today's default. Revoking a
profile-use grant denies future selection/rebind; disabling a profile is the separate delivery cutoff.

    MachineAllowed(actor,m,op) = AuthenticatedMachineChannel(actor,m)
      AND CanonicalAccountOperation(op)
      AND MachineRights(actor,m,RequiredRight(op))
    Select(w) = authorized explicit profile override, otherwise Default[m,w.harness]
    Deliver(w,f,v) requires recorded Bind[w].profile=f, compatible harness/machine,
      active workload and profile, matching expectedAccount and binding revision,
      and an authorized current delivery context at the final delivery admission.

For mailbox delivery that context includes `Valid(p)` and `p.subject=w`; a trusted supervisor adapter instead
needs an explicitly fenced daemon-owned runtime binding. Do not pretend the current Codex shim already checks
broker generations. The proposed service serializes delivery admission with logout/rebind/profile disable,
then permits already-delivered tokens to live until upstream expiry/revocation.

| Proposed transition | Preconditions | Effects / proposed ordering point |
| --- | --- | --- |
| BeginLogin / CancelLogin | manage-login on m; supported harness adapter; expected profile revision; at most one active attempt per profile | Bind expiring attempt to machine/profile/intended actor/revision/epoch in private staging; cancel retires it. Native completion is not yet profile commit. Pending/late completions cannot overwrite a newer revision, logout, or cancelled attempt. |
| CommitLogin | Matching live attempt and revision; acquired credential verified as far as provider supports; management grant revalidated | Commit secret reference plus safe metadata/version in private broker state. Preserve old ready profile on acquisition failure. Unexpected account creates a replacement profile requiring explicit selection, never mutates existing workload account binding. |
| SetDefault | manage-login; same machine/harness, central profile only (legacy sources retired), expected default revision | Commit Default only (always the central profile). This is the linearization point against concurrent CreateWorkload; fail rather than pick an arbitrary racing value. |
| CreateWorkload | Existing workload-create authorization plus right to use selected profile; selection valid, or explicit blocked/unconfigured state | Resolve and persist Bind with workload creation before launch, including default/override origin and expected account. A later launch reads Bind, not the latest machine default. No valid default means needs-login, not another account fallback. |
| RefreshCredential | Same verified provider/account/tenant; current profile/revision/epoch; adapter supports refresh | Serialize refresh, advance v, keep f/u/Bind fixed; commit only if logout/rebind did not supersede operation. Unknown identity/token replacement requires explicit replacement semantics. |
| RebindWorkload | Authorized workload management **and** manage-login; expected binding revision | Optional: stop the workload so its next start launches on the current central login now. Bindings already follow central (P11); the restarted Codex process resumes the same thread under the current account (P17). Failure leaves blocked/rebind-pending, never silent old/new fallback. |
| Logout / DisableProfile | manage-login; expected profile revision | Commit delivery-disabled epoch first, cancel attempts/refresh publication, invalidate quota freshness; then native logout/private-secret removal. Track external failure as cleanup-pending; local issuance remains disabled. Existing bound workloads become needs-login; no automatic account switch. Do not log out host/shared external stores without explicit scope/impact consent. |
| ObserveAccount / ObserveQuota | read-account-status for caller; trusted broker adapter or authenticated scoped session reporter; matching profile/account revision | Store safe allowlisted metadata with provenance/times. Discard delayed results for replaced account/profile binding; unknown account stays unattributed. Session-provided telemetry is advisory, not authority or billing enforcement. |
| Restart broker | Recover profile/default/Bind and disabled epochs from durable state | Reconcile incomplete login/rebind/logout by recorded intent; do not auto-replay acquisition or resurrect logged-out profiles. Replace BootID without changing machine/profile/account identity. |

Credential refresh, profile logout, broker-key revocation, and provider-side token revocation are separate
transitions. Profile logout denies future delivery to all bound workloads; revoking one session principal does
not log out the machine. External token invalidation is reported only when confirmed by the provider. Live
host/shared/environment sources cannot guarantee stable account identity if external programs change them;
label them explicitly and block mismatches when detectable. Migration should record legacy bindings/unknowns
for review, never assert that a missing profile field proves the current default was originally used.

### 9.2 Shared TUI/web service and secret-free projections (P)

API names below are logical operations, **not existing wire endpoints**. Local TUI uses authenticated host
transport; web uses an explicitly granted machine adapter with authority revalidated at execution after relay
delays. Account status permission is distinct from login management; no inference from workload steering,
machine visibility, shared-pool membership, or generic administrator status.

| Logical API / right | Request and safe result |
| --- | --- |
| AccountStatus / read-account-status | Machine/harness/profile selector; returns mode, default, known account label/ID/plan, verification source, readiness, credential-version metadata, observedAt, stale/unavailable reason, and safe binding summary. No login URL or secret. |
| AccountLimits / read-account-status | Returns per provider/account/bucket snapshot: optional used/remaining/limit/reset data, observedAt, receivedAt, source, freshness deadline, confidence, and unavailable reason. Unknown/absent/stale is never presented as unlimited; do not sum shared-account quotas across sessions or profiles. |
| BeginLogin, LoginProgress, CancelLogin / manage-login | Profile revision and attempt ID; bounded status and approved provider authorization URL only to the initiating authorized operator. Expiring flow data is sensitive, excluded from generic snapshots/logs and durable relay payloads; account credentials never become UI/API output. |
| SelectDefault, Logout, DisableProfile / manage-login | Expected revision, explicit target and operation ID; returns applied revision/status and affected-workload counts. Remote timeout means reconcile by operation ID, not repeat login/logout blindly. |
| RebindWorkload / manage-login + workload management + use-profile | Explicit old/new binding, restart timing, expected revisions; returns pending/applied/blocked state. Existing workload never follows a new default implicitly. |

Both clients show machine and harness, configured default, effective workload binding/override, account
identity confidence, login state, and independently fresh limit windows. Machine-offline,
protocol-unsupported, needs-login, stale-data, and operation-indeterminate are separate states. Cached
identity is labeled stale; unsupported/offline adapters disable acquisition, with no shell-command fallback.
Disconnect cancels the attempt where possible and retires its local commit authority; late callbacks cannot
commit. Explicit profile disable/logout closes future delivery, marks affected workloads, and reports pending
remote cleanup without asserting revocation. Native Claude setup-token currently prints a secret in its
terminal; that output must **not** be streamed into web/TUI logs. A secure adapter must capture credentials
privately or report host-assisted/unsupported acquisition; it must not claim browser login parity before such
an adapter exists. Secret input, if supported, uses a write-only protected channel, never reflected into
results or stored in the durable control relay. No raw upstream errors, auth files, cookies, refresh/access
tokens, or credential-bearing URLs appear in ordinary UI/API/log projections.

Quota attribution requires the provider/account that produced the observation, not today's default account.
Codex native account limits and Claude statusline observations have different provenance; session credentials
authenticate the reporting session, not truth of the measurement. Missing account evidence yields unknown
attribution and cannot update a verified machine-account total. Redaction, bounded schemas, and authorization
apply equally to status pushes and reads.

### 9.3 Broker compatibility and current-to-desired gaps

**P:** Preserve the current session trust unit and wire namespaces. Formalize authority as
`(subject,key,credentialGeneration)` and freshness/correlation as separate typed boot/runtime/request values.
Keep daemon-derived roles and explicit host restore. Do not introduce per-pane principals or cryptographic
delegation chains without a concrete requirement and migration plan.

**P:** Define a family of bounded admission events `Admit(p,op,target,point)`. For each audited path, require
`Valid(p) AND Allowed(p,op)` at that point and serialize relevant policy/revocation mutations with it. Already
admitted external effects may finish after later revocation. A proposed ghost `policyVersion` can help reason
about changes; it is not a required wire field or existing epoch.

**P:** For compatibility, initially define read authorization at its final authorization decision, allowing
the already-admitted response to be published or observed later. Describe token delivery as disclosure. If
product requirements instead demand no newly published responses after revocation, add and verify a
publication admission gate in a separate implementation change; client-observation revocation remains
impossible for copies already readable by the session.

| Topic | Current evidence | Proposed contract / gap | Compatibility |
| --- | --- | --- | --- |
| Machine login ownership | One local central store/provider per harness; web machine is a registration | Map authenticated registrations to one broker installation/OS-user/config domain; one default profile per harness, separate account and credential revision. | Legacy modes are retired by a one-time forward migration (no credential copy). |
| Workload binding | Account store v2: every binding follows central; per-runtime delivered evidence and launch fences | Next process launch takes the newest configured central login; running runtimes keep their snapshot (P11, P17). | One-time v1 → v2 migration (original kept as `accounts.v1.json`); no indefinite legacy mode. |
| TUI/web account service | CLI/host Codex actions; no account UI/control protocol in audited machine pane | Shared broker read/manage service, separately scoped permissions and delayed-dispatch revalidation. | Add negotiated capability; old clients remain unsupported rather than invoking arbitrary commands. |
| Subscription limits | Token/context usage; no typed machine quota summary | Account-scoped snapshots with source/times/freshness; unknown stays unknown. | Preserve existing usage events; add a distinct model. |
| Login/refresh/logout | Claude replace/restart; Codex refresh continuity and internal Logout | Correlated attempts and revision checks, stable account refresh, disabled epoch before logout cleanup, explicit account replacement. | Preserve acquisition methods where secure; no secret-bearing generic RPC. |
| Identity typing | Several unrelated fields called generation/request ID | Document disjoint types; future internal wrapper types may prevent accidental comparison. No wire rename required. | Preserve wire format. |
| Session authority | All credential-reading processes act as one session | Make this the explicit security unit. Runtime generation provides stale-work fencing only. | Preserve shared tabs/tools. |
| Replay | Bounded expiring ledger, boot-bound signatures | Promise at-most-once admission for retained identities; require fresh IDs for logical attempts; retain indeterminate outcomes. Permanent idempotency/result lookup would be separate work. | Preserve current retry behavior. |
| Revocation cut | Authority lock plus selected daemon gates; effects may be queued | Enumerate and audit every admission point and all relevant mutation paths before claiming universal serialization. Exclude rollback of previously admitted external effects. | Clarifies semantics; fixes only after audit/review. |
| Response cut | Last authorization check precedes transport publication | Adopt decision-time semantics initially, or explicitly choose a new publish-time contract. Do not claim either retracts delivered data. | Decision-time contract matches observed ordering. |
| Crash durability | Visible rename, sync confirmation, staged publication, separate SQLite state | Specify success, pre-rename failure, visible-but-unsynced error, and restart recovery separately. Prove recoverable intermediate states before promising whole-operation atomicity. | Preserve existing recovery paths. |
| Restore isolation | Drains completion and stops tracked handles before successor | Require verified quiescence of the intended credential-reader set; document assumptions about descendants and stable mounts. | Preserve explicit host regrant; stronger containment needs audit. |
| Archive cleanup | Completion aborts kill on revoke error; reconcile logs error then kills | Explicitly distinguish policy denial, key invalidation, and process cleanup. Decide whether to standardize error handling; do not infer revocation from runtime death. | Documentation first; behavior change undecided. |
| Delegation/grant shrink | Persistent creation ceiling; membership controls management/history; mounts independent | Treat grants as explicit operation/resource rights with documented prospective effect. Decide separately whether changes should cascade to existing children or mounted resources. | Preserve existing children by default pending review. |
| Provider | Local provider seam defaults deny; remote bearer path is separate | Keep separate adapters/trust domains until a reviewed mapping defines subjects, capabilities, and revocation. | No implicit elevation or protocol unification. |
| Liveness | Timers, polling, bounded queues, persistence failures | Conditional eventual cleanup/recovery only with fair scheduling, responsive runtime termination, and eventually successful storage. No unconditional completion deadline under failures. | Preserve bounded best-effort behavior. |

## 10. Bounded model and review questions

Use one broker owner, two registrations aliasing that owner, two human actors with different rights,
two profiles/accounts per harness, one default each, and two credential versions. Add one host, two
coordinators/workers/repos/boots/runtime incarnations and three broker credential generations. Bound RPC
and approval IDs at two each, including reuse. Keep partial publication and failed transitions explicit.
Model crypto ideally and external effects abstractly; check safety before fairness-dependent liveness.
This finite transition system can later be translated into a checker without introducing a toolchain now.

Questions requiring design review: secure Claude acquisition/identity evidence, profile migration and alias-domain
recovery, capability negotiation, and quota freshness policy remain open, alongside the broker questions below.

1. Which product-visible boundary defines revocation: future admission, response
   publication, or cancellation of already queued work? The latter needs a
   downstream cancellation contract and cannot undo completed effects.
2. Are grant changes prospective only? Should moving a member into a coordinator
   require its existing repos to fit the destination ceiling, and should history
   visibility move immediately with membership?
3. Which descendant processes must be quiescent before regrant, on each OS?
   Is tracked-handle termination sufficient for the intended trust model?
4. Should reconcile's revoke-error/kill behavior be preserved, or should cleanup
   expose separate confirmed-revoked and stopped outcomes?
5. What wall-clock rollback, power-loss durability, and registry backup/restore
   guarantees are required? They affect replay and sticky-revocation claims.
6. Do consumers need durable operation/result IDs beyond short-lived RPC IDs?
   If so, specify them separately rather than promising exactly-once effects.
7. Should provider subjects ever be issued local credentials, or should bearer
   connections remain an explicitly separate capability adapter?

## 11. Source/test evidence appendix and next review step

Harness evidence below comes from a separate source review at
`a225e888084c6880d35484f1452345f9b3e38a93`, rather than a locally inspected checkout. The
proposed additions in section 9 are not existing Harness APIs.

| Audited current fact | Pinned cross-repository evidence |
| --- | --- |
| Creation uses machine identity/global provider configuration, not persisted profile selection; executable availability is not auth readiness | [CreateModals.tsx:364](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/frontend/src/components/rail/CreateModals.tsx#L364), [providers.go:313](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/backend/actions/providers.go#L313), [compute-capabilities.md:28](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/docs/compute-capabilities.md#L28) |
| Web machine is provider registration, not broker/account identity; pane lacks account/login/limits controls | [store/providers.go:29](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/backend/store/providers.go#L29), [DaemonPane.tsx:337](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/frontend/src/components/rail/DaemonPane.tsx#L337) |
| Codex rate-limit updates survive raw session events, without typed machine quota summary | [reduce.ts:130](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/frontend/src/session/reduce.ts#L130) |
| Web socket → durable Postgres relay → provider; authority revalidated at dispatch | [engine/provider/sessions.go:303](https://github.com/Kchymet/harness/blob/a225e888084c6880d35484f1452345f9b3e38a93/backend/engine/provider/sessions.go#L303) |

The cross-repository review additionally reports personal BYOM beta requires immutable owner_user_id and blocks
shared/delegated/admin callers; legacy owner-email/admin and broader read visibility differ. No delegated
account-management operator grant exists. These findings lack a line-complete
authorization proof in this appendix and require entry-point validation before implementation. The desired
service must use explicit per-machine read/manage grants and preserve applicable owner restrictions, not
repurpose those broader legacy rights.

Local auth regression evidence:
[codexauth_test.go:92,250,272,305,317](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/codexauth/codexauth_test.go#L92) covers token redaction,
account continuity, refresh revocation, login cancellation, and internal logout;
[centralauth_test.go:24](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/centralauth/centralauth_test.go#L24) covers secret formatting;
[central_codex_test.go:67](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/central_codex_test.go#L67) covers the current store-row
admission shim, not the proposed delivery epoch.

Existing tests are examples and regression coverage, not formal verification:

- [authority_test.go:133,217,563,633,948,1023](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/authority_test.go#L133):
  replay/restart, staged publication, exact regrant, uncertain commits, expiry.
- [sessionrpc_test.go:457,507,1308,1373](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/sessionrpc/sessionrpc_test.go#L457):
  response failures, rotation/reconnect, indeterminate in-flight restart.
- [session_dispatch_test.go:228,444,637,704,981](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch_test.go#L228):
  archived restart cleanup, acknowledged completion, sticky revocation,
  replacement runtime protection, expiry-only renewal.
- [session_dispatch_deferred_test.go](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/daemon/session_dispatch_deferred_test.go):
  deferred action admission checks; [policy_test.go](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/access/policy_test.go)
  and [grants_test.go](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/internal/store/grants_test.go): role and grant cases.

The repository uses Go tests and separate root/harnessproto module checks
([Makefile:8–14,38–39](https://github.com/Kchymet/agent-multiplexer/blob/e6e4d4aaf9fcd3e962713a57441602503b61755b/Makefile#L8)); no heavyweight modeling dependency is introduced here. Before
shipping, review the E/A/P classifications, choose the revocation/disclosure boundary, and agree the initial
invariants. Then trace each selected transition to all production entry points and turn any counterexamples
into focused tests or a small executable model. Runtime and protocol implementation should follow a separate
review of the selected contract and migration plan.
