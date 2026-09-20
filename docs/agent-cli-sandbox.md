# Agent CLI access from protected sessions

`amux agent ...` uses the signed regular-file mailbox already implemented in
`internal/sessionrpc`. Linux keeps both bubblewrap and the harness command
sandbox enabled. macOS uses amux Seatbelt with harness approval controls; its
inner harness OS sandbox is disabled because Seatbelt cannot be nested. No network permission, socket exception, external tool,
or host command runner is needed for these commands.

## Existing work and remaining gaps

Workgroup **840db1 — Session daemon access and isolation** supplied the transport,
namespace mounts, telemetry dispatch, event pager, and acknowledged completion.
The implementation builds on merged #136 (file RPC), #137 (own-only namespaces),
and #138 (daemon lifecycle), including their restart/replay and completion receipt
handling. Local worker reports and the final Codex completion acceptance were
inspected before choosing this approach. Their evidence covers their recorded
candidates; it is not validation of a later candidate or the complete namespace.

The dispatcher audit found two remaining gaps: `name`/`label` selected an ID from
`AMUX_WORKGROUP`, and `sessions` scanned only the filesystem visible to its
process. Rename now loads the fixed context and directly uses session RPC.
Discovery now has a separate read-only query, `agent-sessions`. The existing
role-scoped management `sessions` query is unchanged.

## Dispatcher inventory

This table follows `cmd/amux/agent.go:cmdAgent`, including the additional existing
`capture` and `events` commands. It describes implemented commands, not planned
protocol channels.

| Command / alias | Arguments and supported flags | Execution and failure semantics |
| --- | --- | --- |
| `status` | `idle`, `ready`, `waiting`, `running` | Current-generation self report; errors are nonzero. |
| `hook` | Same states; hook JSON on stdin | Same report, best effort; malformed input and daemon failure are swallowed. |
| `permission` | `request`, `allow`, `deny`, `clear`; `--request-id`, `--tool`, `--action` with either space or `=` values; `--hook` | Self-only diagnostic observation, never an answerable approval. Explicit forms return errors, including invalid observation lifecycle. Generated `--hook` and legacy stdin-only forms remain nondisruptive. |
| `model` | Model name; `--statusline`; `--forward-base64` with space or `=` value | Explicit model errors are nonzero. Status-line JSON reporting is best effort and wrapped-command input is forwarded byte-for-byte. Forwarding runs in the caller's sandbox. |
| `capture` | Event name; `--hook`; legacy stdin-only form | Daemon derives and opens the caller's Claude transcript. Generated forms are best effort. Explicit capture errors for unsupported runtimes remain errors. |
| `name`, `label` | Display-name words, joined by spaces | Fixed-context self rename; success requires daemon acknowledgement. No environment-selected target. |
| `done` | No arguments or ID flags | Fixed-context self archive; success requires a successful durable result. |
| `sessions` | `--json` | Cross-session conversation discovery, retaining the existing row fields and most-recent-first text/JSON output. |
| `events` | Optional session ID; `--json`; `--after` or `--cursor`, with space or `=` values | Existing bounded normalized-event query; role-based read authorization is unchanged. |
| Empty command, `help`, `-h`, `--help` | None needed | Local help, independent of daemon availability. |

Deprecated top-level `amux name` and `amux hook` share their agent implementations.
As before, words following `name` are the name, not ID-selection flags. Existing
`sessions` argument handling recognizes `--json`; it does not acquire new flags.

## Authority and discovery

Linux mounts `/amux-session-access` read-only. macOS grants the original
daemon-private credential path read-only and locates it with
`AMUX_SESSION_ACCESS`; the locator itself conveys no authority. Signed credentials identify
one store subject. The mailbox and responses are read-only; only its `requests/`
overlay is writable, beneath the session workspace, where the harness's ordinary
workspace-write policy can publish requests. The daemon private parent, store,
broad socket, sibling mailboxes, and sibling writable trees stay hidden. Merely
making a socket visible would not authorize a sandboxed `connect` syscall.

Mutations remain authorized against the signed subject and current daemon-owned
store row, immediately before their effect. Changing `AMUX_WORKGROUP`,
`AMUX_WORKSPACE`, UUIDs, cwd, or hook JSON cannot change self-report identity.
Foreign rename/archive and ordinary-agent control-plane operations remain denied.

`agent-sessions` is the intentional read exception: any active authenticated
session can discover the existing host/user and managed Claude/Codex conversation
metadata. It accepts no subject ID, target, path, or action fields. It does not
relax management, normalized-event, raw-transcript, or filesystem permissions.
Returned paths are descriptive metadata, not a mount or a read capability.

The CLI collects internal pages of at most 64 KiB before printing. Continuations
encode a lexical `(harness, path)` position, used only for comparison with rows
already enumerated from daemon-selected homes, never as a path to open. Each page
rechecks authorization before reading and before release. Listings are live,
like directory enumeration: a newly created path before the cursor is visible on
the next invocation; changing mtimes cannot duplicate rows across pages. A row
that cannot fit is an explicit error, never silent truncation.

Discovery pins directories and opens regular files through `hostprep`, rejecting
symlink components, hardlinks and special files. Cwd extraction reads at most
1 MiB per transcript; missing/malformed metadata leaves cwd empty. Unreadable
homes/files remain best-effort, as in the original diagnostic listing.

## Availability, restart, and completion

The existing sessionrpc request/claim/response protocol provides unique request
IDs, atomic publication, authenticated acknowledgements, bounded concurrency,
expiry, duplicate rejection, and signed daemon service generations. Clients can
rediscover service after restart before publication. An already claimed mutation
with a lost result is indeterminate and is never blindly replayed as a new action.
Telemetry wrappers swallow those errors; explicit commands report them.

`done` uses the daemon's self-completion path: commit archival in the store,
publish the successful response, accept the narrow receipt, then stop/revoke the
runtime with a bounded abandonment fallback. Exit zero is not a queued request
or optimistic local write. Restart recovers archived cleanup from the store.
Restoring a session requires host regrant; a revoked old credential cannot mutate
the restored incarnation. Old processes without the fixed context must be
relaunched: see [namespace rollout](namespace-rollout.md).

## Validation

`TestAgentNamespaceCommandSurface` builds the real CLI, uses the production
`panespec.Resolve` platform sandbox, and serves the production daemon file RPC
against isolated SQLite state. It exercises the dispatcher, hook forwarding,
rename aliases with changed/unset environment IDs, foreign discovery, denied
foreign/control-plane requests, concurrent reports, daemon authority restart,
retry, and acknowledged durable archive. It never starts the host daemon.

Run with bubblewrap 0.12+ on Linux, or from a host terminal on macOS
(macOS cannot nest Seatbelt). On macOS use `TMPDIR=/private/tmp` for short,
canonical test paths:

```sh
AMUX_REQUIRE_NAMESPACE_TEST=1 go test ./internal/daemon \
  -run '^TestAgentNamespaceCommandSurface$' -count=1 -v
```

The transport, lifecycle, policy, paging and hostile-file regressions are also
covered by their package tests. `TestAgentRuntimeCommandSurface` additionally
runs the same script through real Claude Bash, interactive Codex on a PTY, and
Codex App Server with the production Supervisor. It uses a local deterministic
provider, synthetic conversations and credentials, and requires the actual tool
result to report success. On Linux an outer-writable ephemeral file must become unwritable
inside the harness tool sandbox. macOS tests the inherited amux Seatbelt policy
and verifies that protected authority remains read-only through actual tools. The provider's final text is not an acceptance
signal. CI pins the runtime archives and verifies their checksums. See the
[validation record](agent-cli-validation.md) for versions, results and reproduction.
