# Security and trust boundaries

amux is an experimental developer tool that runs coding agents with access to
selected local files, accounts and network services. Its public source and MIT
license are not a security certification. The isolation tests cover specific
boundaries; they do not establish safety against every malicious workload.

## Intended environment

The host operator, daemon, installed amux executable, selected harness binaries
and OS are trusted. Local protected sessions use Seatbelt on macOS and
bubblewrap on Linux/WSL2. Launch fails when the required backend is unavailable
or disabled. Native Windows is unsupported.

Session processes receive their own worktree and private harness configuration,
explicit account/tool grants, and read-only system/Git object resources. Direct
access to other sessions, host control credentials and broad host state is
restricted. Coordinators and the console receive broader, authenticated daemon
operations according to their roles; they are more privileged than workers.

## Deliberate grants and limits

- **macOS host applications:** native browser authentication permits
  LaunchServices `lsopen`. It can launch other host applications outside the
  session's Seatbelt profile. The profile does not confine those applications.
  This is a substantial exception to the direct filesystem/process restrictions;
  do not treat macOS sessions as a boundary for hostile tenants.
- **Shared network:** sessions can access IP services, including reachable local
  services, and can bind/listen on IP ports. There is no per-session network
  isolation or general prevention of data exfiltration.
- **Accounts:** selected model credentials, Git/GitHub authentication and MCP
  credentials are intentional capabilities. The macOS broker restricts which
  host account items it serves; it does not reduce the upstream account's
  permissions or prevent a recipient from using disclosed credentials. Claude
  login/logout can change the shared host account. Use accounts appropriate for
  the repositories and services the agent should reach.
- **Configuration:** copied harness settings can contain inline secrets. Hooks,
  plugins, MCP servers, shell rc files and editor configuration can execute code.
  Configuration copying does not sanitize them; promotion can affect subsequent
  host and session launches. Review template contents and proposed promotions.
- **Approval defaults:** Claude automatic approval and Codex's automatic reviewer
  can authorize actions without a human prompt. They complement the OS policy;
  they are not proof that a command or generated change is safe.
- **Platform differences:** Linux has a private PID namespace and `/proc`;
  macOS has Seatbelt restrictions on process inspection/signals, with no private
  PID namespace. The trusted host user can access local session data.

See [sandbox configuration](docs/sandbox-config.md),
[agent command authorization](docs/agent-cli-sandbox.md), and
[validation evidence](docs/agent-cli-validation.md) for the implemented grants
and checks.

## Remote access and data sharing

The optional compatibility relay requires TLS and a nonempty bearer token,
including over its Unix socket. Standalone raw-stdio `amux harness` is disabled.
Provider connections require TLS verification and a bearer token.

Provider capabilities are separate grants:

- `--publish-sessions` publishes inventory and allows session lifecycle/steering
  commands. Starting agents or sending prompts can cause code execution through
  those sessions even with raw compute disabled.
- `--read-only-sessions`, with publication enabled, rejects lifecycle and
  steering commands. It does not hide the published inventory.
- `--runtime-events` additionally publishes normalized transcript content,
  including model text and tool arguments/results. Transcripts and pane I/O can
  contain secrets; no general redaction guarantee is provided.
- `--allow-compute` grants arbitrary process execution as the provider's host
  user. Those requests do not have to use amux's protected session launcher.

Trust the orchestrator with the capabilities you enable. The outbound connection
avoids an inbound provider port; it does not limit the authorized remote actions.
See the [provider trust table](docs/remote-provider.md#trust-model--read-this-first).

## Builds, upgrades and local state

Build the application with **Go 1.26.8 or a newer patched release** and run
`make vuln` for both Go modules. This minimum excludes older standard-library
vulnerabilities in code paths amux uses, including
[GO-2026-4970](https://pkg.go.dev/vuln/GO-2026-4970) and
[GO-2026-6090](https://pkg.go.dev/vuln/GO-2026-6090). Keep the build toolchain
patched: Go's standard library is compiled into the resulting executable.

Installing a new binary does not change a running daemon, pane or App Server.
Rebuild/reinstall, restart the daemon, reopen dashboards and restart any provider
service. Follow the [namespace rollout guide](docs/namespace-rollout.md) when
moving from an older isolation/layout scheme. Back up session work before
deletion or recreation; a rejected migration is not permission to discard it.

Keep credentials, transcripts, runtime databases and local config out of public
issues and commits. `.gitignore` is only an accidental-add guard. If a real
credential is exposed, revoke/rotate it; deleting the file from the current tree
does not remove copies from Git history or elsewhere.

## Reporting a suspected vulnerability

For a sensitive report, open a minimal GitHub issue requesting a private contact
channel, without publishing the exploit, credentials or private session data.
If GitHub offers **Report a vulnerability** on the repository's Security tab,
use that private channel instead. Do not assume an ordinary issue is private.

In the private report, include the commit/build, OS, Go and harness versions,
relevant launch settings, expected boundary, and a minimal reproduction using
synthetic data. Distinguish an unintended boundary bypass from one of the
deliberate grants above. Avoid testing with another person's account or data.
