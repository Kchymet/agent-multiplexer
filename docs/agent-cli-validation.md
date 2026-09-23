# Agent CLI candidate validation

Date: 2026-09-13. Branch: `amux/291907-bedb20`. Implementation: `f4b16df`,
merged with upstream `1caab4a` at `81ff6f7`, plus the runtime acceptance and CI
fixtures included in this PR. These results cover this candidate's complete
command surface, not the earlier workgroup 840db1 completion-only candidates.

## Real runtime acceptance

| Launch path | Runtime | Result |
| --- | --- | --- |
| Claude agent launch, ordinary Bash tool | Claude Code 2.1.263 | Passed |
| Interactive Codex agent launch on a PTY, ordinary `exec_command` | Codex 0.153.4 | Passed |
| Codex App Server through the production Supervisor, ordinary `exec_command` | Codex 0.153.4 | Passed |

`TestAgentRuntimeCommandSurface` shares the complete command script with
`TestAgentNamespaceCommandSurface`. It builds the CLI, obtains real signed
session credentials from an isolated authority, serves the production mailbox
and daemon dispatch against SQLite, and launches through production `panespec`.
The only executable bind substitution replaces the Go test image with that
compiled CLI at the production trampoline and hook paths.

The local provider deterministically requests a real tool call. Acceptance
requires the returned tool result to report successful execution and the archive
acknowledgement, plus direct assertions of daemon-owned state. A synthetic final
model message cannot pass the test. Claude runs with `sandbox.enabled` and
`failIfUnavailable`, with unsandboxed fallback disabled; its permission rule
allows only the exact fixture script. Both Codex modes use the production
workspace-write and approval defaults; the requested tool execution specifies
`use_default`. No test tool call requests escalation.

An ephemeral root file is first written inside the outer namespace. The tool
must read its contents and fail to write the same file, proving an additional
inner filesystem boundary. The fixed authentication context must remain
read-only, the peer filesystem hidden, and foreign/control-plane writes denied.
No broad daemon socket is mounted into the tool sandbox.

The script covers the documented dispatcher inventory, aliases and flag forms,
model forwarding, authoritative telemetry/capture, rename with changed/unset
environment IDs, text/JSON foreign discovery, bounded events, concurrent reports,
offline request publication before authority restart, reconnection and explicit
retry, confirmed durable archival, and rejection of repeated completion. Explicit
Claude-only capture still fails on Codex; generated capture remains best effort.
Codex resumes a synthetic persisted UUID, while Claude starts a fresh pinned UUID.
No host conversations, live model credentials, or running host daemon are used.

## Checks and environment

- Full `make test`, including the separate `harnessproto` module: passed.
- `make vet`, both modules: passed.
- `make build GOFLAGS=-buildvcs=false`: passed. This only omits Git build metadata.
- Namespace and all three real runtime acceptance paths with `go test -race`:
  passed.
- `git diff --check`: passed.
- Earlier focused CLI/security/discovery/transport race regressions passed,
  including bounded paging, hostile files, duplicate requests, stale generation,
  restart reconnect and indeterminate mutations without replay. Existing
  access/regrant and lifecycle tests cover restored sessions and durable receipts.
- Linux/Darwin amd64/arm64 cross-builds passed on the implementation before the
  upstream merge; subsequent changes in this PR add tests, CI and documentation.

Go is 1.26.4; production outer bubblewrap is 0.12.0. Runtime packages are copied
into each synthetic session so their complete binaries/helpers remain visible
under the real production mounts. Claude also receives the same pinned outer
bubblewrap executable in its fixture dependency PATH. Locally, missing `socat`
and `libwrap` were extracted from public Ubuntu packages into this workspace;
only that helper process receives its private library path. CI installs `socat`
on its ephemeral runner. The Codex package includes its paired sandbox helper.

The initial ordinary tool sandbox denied provider and test sockets. A supported,
narrowly approved test-runner escalation resolved that environment limitation.
The runner itself uses an additional bubblewrap wrapper with a read-only root,
only this workspace and `/tmp` writable, a synthetic HOME/environment, and the
live `/amux-session-access` hidden. This permits the fixture's local services;
it does not remove the production outer namespace or either real harness's
command sandbox. Network access was also narrowly approved for public fixture
and Go dependency downloads and GitHub operations. CLI access itself remains a
file-mailbox operation requiring no network exception.

The former socket failures and unavailable real-runtime gates recorded on
2026-09-12 are superseded by these successful runs. Live paid provider/model
behavior and an interactive Claude UI are not exercised: Claude's actual Bash
runtime uses print mode, and both providers are local deterministic fixtures.

## Reproduction and CI

Install bubblewrap 0.12+, Go dependencies, and `socat`; extract the pinned native
packages below. No authenticated harness account is necessary.

```sh
export AMUX_REQUIRE_NAMESPACE_TEST=1 TERM=xterm-256color
export AMUX_TEST_CODEX_PACKAGE=/absolute/path/to/extracted/codex-package
export AMUX_TEST_CLAUDE_BIN=/absolute/path/to/claude/package/claude

go test -race ./internal/daemon \
  -run '^TestAgentNamespaceCommandSurface$' -count=1 -timeout=120s -v
for runtime in claude codex codex-app-server; do
  AMUX_TEST_AGENT_RUNTIME="$runtime" go test -race ./internal/daemon \
    -run '^TestAgentRuntimeCommandSurface$' -count=1 -timeout=120s -v || break
done
```

When running from an existing managed agent, hide its live fixed session context
and use a synthetic HOME before running the broader suite. If the parent command
sandbox denies local fixture sockets, use only the supported narrow test-runner
approval path; do not disable the tested harness sandbox. Optional
`AMUX_TEST_SOCAT_ROOT` supplies an extracted Ubuntu package tree when system
`socat` is absent.

CI's `namespace-runtime` job installs the pinned bubblewrap runner, verifies
these archive digests, and runs the namespace fixture and all three real modes
without permitting skips. See the PR's checks for the current remote result.

| Archive | SHA-256 |
| --- | --- |
| OpenAI `rust-v0.153.4/codex-package-x86_64-unknown-linux-musl.tar.gz` | `a822187e1a2420c61c5926721bfbd878701ed95547c9bb0d4de4498a16ba1821` |
| npm `@anthropic-ai/claude-code-linux-x64` 2.1.263 | `8b6207348ad56fdcde085a0ad1f7cff0dfe06ce2c6c1bf97f69f1a1a7b6d0945` |

Local raw logs: `/tmp/amux-bedb20-final-checks.log` and
`/tmp/amux-bedb20-runtime-{claude,codex,codex-app-server}.log`.

## macOS platform validation — 2026-09-20

The macOS implementation was exercised locally on macOS 26.5.2 arm64. These
results apply to the platform changes in this working tree; the historical
Linux candidate record above remains separate.

| Check | Result |
| --- | --- |
| `make test` (root module and `harnessproto`) | Passed |
| `make vet` (both modules) | Passed |
| `make cross` (Linux/macOS × amd64/arm64) | Passed |
| Real Seatbelt files, hardlinks/symlinks, inherited descriptors, host signals | Passed |
| Read-only Codex worktree with writable private state/mailbox | Passed |
| Own/peer and future Unix socket boundaries | Passed |
| Git checkout reading immutable objects, denied pool metadata/writes | Passed |
| Compiled CLI mailbox, spoofed/cleared locator, denied host control | Passed |
| Claude Code 2.1.278 ordinary Bash command | Passed |
| Codex 0.155.1 interactive PTY `exec_command` | Passed |
| Codex 0.155.1 App Server/Supervisor `exec_command` | Passed |

All three real harness modes use the same complete CLI command/restart/archive
fixture described above, through the production Seatbelt launcher. The pinned
native archives were downloaded, hashed, and tested locally. The fixture uses
synthetic credentials and a local deterministic HTTP provider; no host accounts,
conversations, paid model calls, or interactive Claude UI are exercised.

macOS cannot apply nested Seatbelt profiles. The launcher disables Claude's
inner sandbox and gives Codex an inner `danger-full-access` policy while
retaining **amux's inherited OS sandbox and harness approval controls**. The
Linux test's additional inner-write-denial probe is Linux-only. macOS instead
verifies actual tool calls can use their own workspace and signed mailbox while
credentials remain read-only and peer/host operations fail. Direct Seatbelt
regressions also exercise a custom HOME under a system read grant.

`make test` works with the normal macOS environment. Relevant fixtures use a
short canonical `/private/tmp` root to avoid `/var` symlink traversal and
Darwin's Unix socket pathname limit. No-follow production checks remain intact.
Running these tests from an already sandboxed agent requires a host test-runner
approval because macOS cannot nest Seatbelt.

CI now includes a macOS build/test job and a `seatbelt-runtime` job with all three
pinned runtime modes, in addition to the existing Linux namespace job. Those
remote CI jobs have not been executed as part of this local validation.

| macOS arm64 archive | SHA-256 |
| --- | --- |
| OpenAI `rust-v0.155.1/codex-package-aarch64-apple-darwin.tar.gz` | `e6e08717da9e35b72332eff753527fe79a9ae876081033c5c6820a8e5f58b943` |
| npm `@anthropic-ai/claude-code-darwin-arm64` 2.1.278 | `934a258c59e90ed6ba768d0d46502d3163607dec949845650af80e5f1228330c` |

Reproduction: extract these archives, set `AMUX_TEST_CODEX_PACKAGE` and
`AMUX_TEST_CLAUDE_BIN` as above, and run the same three-mode loop on a macOS host.
Linux runtime execution must use the Linux CI job or a suitable Linux/WSL2 host;
it was not repeated on this Mac.

## macOS networking and browser follow-up — 2026-09-20

The localhost provider tests above did not exercise the macOS DNS broker.
Before this follow-up, unauthenticated HTTPS requests to Anthropic, OpenAI and
Linear MCP succeeded on the host but failed DNS resolution under Seatbelt.
Kernel logs also recorded the same denied `mDNSResponder` connection for the
running Claude and Codex processes. Allowing that exact system Unix socket
restored certificate-verified HTTPS to Anthropic, OpenAI, ChatGPT's Codex API
and Linear MCP; no credentials or paid requests were used.

A native DNSService connection regression now runs in the ordinary macOS CI
suite. Browser discovery also runs by default. Opt-in browser acceptance was
verified locally with actual HTTP callbacks from the default browser, through
both `/usr/bin/open` and the AppKit/NSWorkspace API used by native harnesses.
The host explicitly approved native LaunchServices access after being informed
that `lsopen` cannot be restricted to browsers or URLs and permits launching
host applications outside Seatbelt. Documentation and doctor report this grant.

## Public sharing review — 2026-09-23

Review base: `c88b7d6` on `master`, plus the documentation and patched-toolchain
changes accompanying this record. This is a bounded source/documentation and
dependency review, not an independent penetration test.

- Gitleaks 8.30.1 found no secrets in the current checkout. A scan of the fetched,
  non-shallow Git history (`git --log-opts='--all'`) reported one generic-key
  match in a coordinator-model flag test on a development branch. Inspection
  confirmed it was a `--coordinator-model` argument, not a credential. No real
  secret was identified; scanner reports were redacted and kept outside the repo.
- GitHub secret scanning and push protection were enabled; its secret-alert API
  returned no alerts. Dependabot alerts and private vulnerability reporting were
  disabled at review time. The latter has a contact-request fallback documented
  in [Security](../SECURITY.md#reporting-a-suspected-vulnerability).
- Govulncheck 1.8.0 with the host's Go 1.26.4 found reachable standard-library
  advisories GO-2026-4970, GO-2026-5856, GO-2026-5972 and GO-2026-6090 in the root
  module. The application now requires Go 1.26.8. `make vuln` with that toolchain
  reported no vulnerabilities in either module; the same check runs in macOS
  and Linux CI. Existing executables require rebuilding and process restarts.
- Setup and security claims were checked against the launcher, access policy,
  credential broker and provider flag implementations. The guides now distinguish
  direct sandbox restrictions from macOS host-app launching, account/network
  grants, remote session control and arbitrary provider compute.

To repeat the checks with Gitleaks installed:

```sh
gitleaks git --redact --log-opts='--all' .
gitleaks dir --redact .
make vuln
```

Review findings instead of suppressing all matches in test files. A clean scan
only covers the fetched references, scanned files and known rules/advisories at
that time; it does not prove that no sensitive information has ever been shared.
