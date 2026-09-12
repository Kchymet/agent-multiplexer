# Agent CLI candidate validation

Date: 2026-09-12. Base: `88a7d6d` (merged lifecycle #138). Branch:
`amux/291907-bedb20`. These results apply to this branch's implementation, not
the earlier workgroup 840db1 runtime candidates.

## Passed

- `TestAgentNamespaceCommandSurface`, including a race-enabled run: compiled CLI,
  real production bubblewrap mounts, signed file transport, daemon dispatch, and
  isolated SQLite. The test covers the command inventory, aliases, forwarding,
  authoritative telemetry/capture records, renamed self with changed/unset IDs,
  foreign discovery and denied foreign writes/control operations, concurrent
  requests, offline publication before service/authority restart, retry, confirmed
  durable completion, and rejection of a repeated completion.
- Focused CLI, discovery paging, hostile-file, report, completion and transport
  regressions, including race checks. The transport cases include strict JSON,
  duplicate envelopes, rotation, restart reconnect, and indeterminate in-flight
  mutations without replay. Existing access/regrant tests passed in the package
  suite. Discovery tests cover 1,000 rows across multiple bounded pages, keeping
  valid rows after hostile files, and suppressing partial CLI output on failure.
- Full package tests for `internal/access`, `internal/agent`, `internal/core`,
  `internal/claudecfg`, `internal/codexcfg`, `internal/hostprep`,
  `internal/sessionrpc`, `internal/wsops`, and the separate `harnessproto` module.
- `make vet` (both Go modules) and `git diff --check`.
- Linux amd64/arm64 and Darwin amd64/arm64 `make cross` builds.
- `make build` with `GOFLAGS=-buildvcs=false`. Default VCS stamping cannot obtain
  Git status in the nested build environment; this flag only omits build metadata
  and was also used for cross-builds.

The namespace fixture ran from the existing ordinary Codex tool shell with its
sandbox enabled; no escalation was requested. It added the production amux
bubblewrap confinement and did not remove inherited syscall restrictions.
Bubblewrap was the previously recorded 0.12.0 fixture at
`840db1/558e17/.fixtures/bwrap-0.12.0/bin/bwrap`. Go was 1.26.4. Public dependencies
were read from existing local fixture caches with `GOPROXY=off`, `GOSUMDB=off` and
`GOCACHE=/tmp/amux-bedb20-go-cache`; no account credentials or host daemon were
used. Cross-build dependencies were copied into `/tmp/amux-bedb20-modcache`.

Reproduction of the main integration gate, after making bubblewrap 0.12+ and the
Go dependencies available:

```sh
AMUX_REQUIRE_NAMESPACE_TEST=1 go test -race ./internal/daemon \
  -run '^TestAgentNamespaceCommandSurface$' -count=1 -v
```

## Unavailable acceptance and broader checks

| Check | Observed limitation |
| --- | --- |
| Real Claude Bash tool turn | Claude 2.1.263 is available in the old fixture, but the current outer command sandbox denies both TCP and Unix socket creation. A local deterministic provider/relay cannot start. No live model turn was substituted. |
| Interactive Codex tool turn | Codex 0.153.4 is available, but the same provider socket limitation blocks a tool-turn fixture. Direct `codex sandbox -c 'sandbox_mode="workspace-write"' -- /bin/sh ...` also failed: `bwrap: loopback: Failed to create NETLINK_ROUTE socket: Operation not permitted`. No network-enabled profile was used to work around this. |
| Codex App Server mode | Unix socket creation is denied (`Operation not permitted`), so the actual App Server endpoint and nested command tool cannot be validated here. |
| Full `make test` | Existing socket tests fail in `cmd/amux`, `codexapp`, `daemon`, `mux`, `panespec`, `provider`, and `wiretls`. `TestScopeRootsMatchBinds` also attempts to create a socket directory under the read-only host data root. The full suite is **not green** in this environment. |
| Remote refresh / PR | `git fetch origin` fails with `Could not resolve host: github.com`; `gh pr view` fails connecting to `api.github.com`. No push, PR, CI, merge, or session completion is claimed. |

The namespace fixture is useful integration evidence, but does not replace real
Claude/Codex launch-path acceptance. Interactive Codex and App Server mode remain
explicit acceptance gates on an appropriately provisioned runner that can start
local test services while keeping its agent tools confined. The earlier workgroup
completion-only runtime evidence has not been relabeled as full-command coverage.

Raw local logs for this run are in `/tmp/amux-bedb20-{test,vet,race,final-focused,
namespace-final,build,cross}.log`. No checks were made green by disabling amux confinement, the
harness command sandbox, authentication, or write authorization.
