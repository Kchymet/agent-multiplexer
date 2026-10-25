# Codex 0.157.1 native readiness correction

## Scope

Correct only the real-runtime command-surface fixture's native Codex readiness
gate. Codex 0.157.1 renders the configured model and approval mode as separate
terminal writes joined by cursor positioning, so matching a contiguous raw PTY
substring can wait forever even though the composer is ready. Preserve every
command-surface assertion, production namespace, nested tool sandbox, synthetic
provider, raw failure diagnostics, older Codex coverage, and Claude coverage.
No production behavior changes.

## Worker and base evidence

- Authenticated `amux status --json` identifies this task session as Codex model
  `gpt-5.6-sol`, task mode, with structured control.
- The private Codex configuration sets `model_reasoning_effort = "high"`. Its
  default model is `gpt-6-astra`; the authenticated session snapshot proves the
  launch-specific `gpt-5.6-sol` override is effective.
- The assigned branch `amux/c22e02-2ee546` was clean at
  `04c3586bacd0aa8fbe9717b7e3aa9204c09da4b6`, then fetched and merged
  `origin/master`; it was already current at the same commit.
- During verification, `origin/master` advanced to
  `06806a9b0ed0e0da2b9c34e2e76a8aec876aff61`. That commit changed the same
  runtime fixture to exercise yielded-process polling; it merged cleanly at
  `47115e8abf27dae326bb056f32f4afa1e3f35507`, preserving both assertions.

## Bounded design and regression

1. Continue capturing every raw PTY byte for unchanged timeout/failure
   diagnostics.
2. Feed those same chunks to the repository's VT emulator, including emulator
   replies to terminal queries, and declare readiness only when the configured
   model and `default` appear as adjacent fields on one rendered row. This
   interprets styling, cursor movement, erasure, and arbitrarily split control
   sequences without encoding any observed cursor coordinates.
3. Send the bracketed paste and submit key in one ordered PTY write after that
   semantic readiness signal; add no timing delay.
4. Unit-test text and a cursor-position sequence split across four writes, using
   arbitrary coordinates and rejecting readiness until the footer is complete.
5. Keep the pinned older Codex and Claude command-surface CI runs, then run both
   native Codex and Codex App Server command surfaces against the official full
   0.157.1 Linux package, SHA-256
   `0e211868c9fd73cb49ad35ac675b5eafdf6b9f453df8a493df980c59a590fe5f`.

## Claim boundary

The fixture uses an empty isolated home and deterministic local provider. It
tests readiness plus the existing production namespace/inner sandbox, CLI
surface, peer discovery, self-only writes, concurrent reports, restart, and
archive acknowledgement. It does not use real credentials, request a paid model,
deploy or restart a live daemon, or broaden product behavior.

## Verification

- Downloaded the official full Linux x86_64 package, verified archive SHA-256
  `0e211868c9fd73cb49ad35ac675b5eafdf6b9f453df8a493df980c59a590fe5f`,
  and observed `codex-cli 0.157.1` from its extracted binary.
- `TestCodexNativeReadinessUsesRenderedTerminal` passed 20 repetitions under
  the race detector. It exercises model text and CSI cursor positioning split
  across four writes and rejects every incomplete state.
- `TestAgentRuntimeCommandSurface` passed with the full 0.157.1 package in
  native `codex` mode (5.73s) and `codex-app-server` mode (4.83s), with
  `AMUX_REQUIRE_NAMESPACE_TEST=1` and `TERM=xterm-256color`.
- The same full native 0.157.1 command surface passed under the race detector
  on the final base in 7.93s test time (9.04s package time).
- Repository `make test` passed for the root module and `harnessproto`; `make
  vet` passed for both modules. Its final-base daemon package took 26.02s.
  `git diff --check` passed.
- The App Server fixture requires a test-runner exception for its loopback
  `httptest` listener in this managed environment. The first restricted attempt
  failed at listener creation before launching Codex; the approved rerun retained
  the production outer namespace and nested tool sandbox.

---

# Codex 0.157.1 Linux socket compatibility

## Scope

Restore supervised Codex App Server connectivity on Linux/WSL2 after Codex
0.157.1 changed Unix listeners from a socket at the requested path to a
rendezvous symlink whose physical socket is under
`/tmp/codex-daemon-<uid>/`. Preserve the accepted runtime behavior from PR #168:
one serving process per exact session, daemon-side reconnect to that process,
native and web clients sharing the same thread, stale-reader fencing, and
process-lifetime ownership.

This change does not share `/tmp`, mount the host Codex daemon directory, expose
sibling sockets, enter another process namespace, or weaken the existing user,
mount, PID, credential, and session boundaries. It does not alter macOS
Seatbelt, browser/Android coordination, licensing, notices, or the separate
broad isolation-dependency work.

## Worker and base evidence

- Authenticated `amux status --json` identified worker `2f26f5` as live model
  `gpt-5.6-sol`.
- The worker's private Codex configuration set
  `model_reasoning_effort = "high"`; the supported normalized turn stream showed
  no per-turn effort override, so effective effort was the configured `high`.
- The branch was clean and refreshed from `origin` before edits. `FETCH_HEAD`
  and `HEAD` were both `b784ab5045b0aeff8fadc3fd1a9d14b8b0f57254`, which
  includes runtime PR #168 and UI PR #169.
- Before publication, the focused change was merged forward to
  `59f44549889ca5fb5df5ac3ded73dc03f5288953`, preserving the accepted notices,
  macOS policy, and machine-login/broker-identity changes from PRs #170–#172.
  This work does not modify the macOS Seatbelt/LaunchServices policy.

## Upstream source and artifact provenance

- Behavior source: OpenAI Codex tag `rust-v0.157.1`,
  `codex-rs/app-server-transport/src/transport/unix_socket.rs`:
  <https://raw.githubusercontent.com/openai/codex/rust-v0.157.1/codex-rs/app-server-transport/src/transport/unix_socket.rs>
- The tagged implementation canonicalizes the requested parent, hashes the
  canonical parent plus requested filename, binds that physical name under the
  per-user Codex daemon directory, then publishes the requested path as a
  symlink. Its guard removes only the expected link and physical socket.
- Real-binary validation will use the official OpenAI GitHub release asset for
  `rust-v0.157.1`, verify its published SHA-256 before extraction, and record
  the archive digest, extracted binary digest, and `codex --version` output.
  The fixture uses an empty Codex home, no account, and no model request.

## Bounded design

1. Keep the existing per-session rendezvous path
   `<amux data>/cx/<session-key>/cx.sock` for old and new Codex versions.
2. Create a short, real mode-0700 physical directory below an amux-owned host
   `/tmp/amux-cx-<euid>` root, keyed by both the amux data instance and exact
   session. In each Linux Codex pane, bind only that session child at
   `/tmp/codex-daemon-<euid>`. The surrounding `/tmp` remains the pane's private
   tmpfs; neither the amux host root nor a sibling child enters the namespace.
   The short host path also keeps Codex's 64-hex-character socket name below
   Linux's `sockaddr_un` path limit.
3. When the host WebSocket client sees a symlink at an amux rendezvous path,
   accept it only if the path has the exact amux session layout and the target
   equals Codex's deterministic physical path for that rendezvous. Map that one
   basename into the session's host physical directory. Before connecting, use
   `linkat` without symlink following to pin the selected socket inode into a
   same-filesystem, host-only mode-0700 dial directory; validate and connect to
   that pinned entry, then remove it. This closes the mutable-leaf race for both
   relocated and older direct sockets. Reject sibling hashes, relative links,
   extra aliases, redirected physical directories, and malformed session-key
   paths. Custom Unix endpoints outside the generated amux tree retain ordinary
   Unix symlink behavior.
4. Leave Supervisor and Manager lifecycle code unchanged. Start still removes
   only the stale rendezvous entry under the per-session start gate; Codex owns
   physical-socket cleanup and its startup lock. Reconnect continues to attach a
   new daemon-side WebSocket to the same live process and pinned thread.

## Verification plan

- Unit-test direct-socket compatibility and exact 0.157.1 symlink resolution.
- Deny sibling-hash, relative-link, noncanonical alias, redirected-source, and
  malformed-layout cases.
- At a deterministic barrier between socket selection and connect, replace both
  a direct and relocated source leaf with a symlink to a second listener and
  prove the connection remains pinned to the originally selected listener.
- Exercise real bubblewrap namespaces to prove an own physical socket is
  connectable while a sibling physical socket and host aliases are absent.
- With the digest-verified official 0.157.1 binary and an empty home, execute the
  production `AppServerCommand` scope, complete WebSocket/JSON-RPC initialize,
  perform the native attach bootstrap from a second protected namespace, drop
  and reconnect the daemon-side transport, and assert the serving PID/process
  and pinned thread were not replaced.
- Run focused packages, race-sensitive lifecycle tests, and the repository's
  required checks. Keep older direct-socket fixtures explicit.

## Claim boundary

The synthetic fixture can establish transport initialization, protected native
attach bootstrap, reconnect, exact-session isolation, and one-process lifetime
against Codex 0.157.1. It cannot establish account authentication, paid/model
execution, full interactive TUI behavior, browser/provider integration, or the
founder acceptance matrix. Named-device/version acceptance and live approval
remain separate.

## Implemented evidence

- Official GitHub release asset:
  `codex-x86_64-unknown-linux-musl.tar.gz`; published and observed SHA-256
  `e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2`.
- Extracted binary SHA-256
  `3e2584f3f3829a43a0495011a1cecb2facbe64a2403e2b682351fd9c2983f970`;
  version output `codex-cli 0.157.1`.
- Old direct-bind compatibility was rechecked with the official 0.154.0 archive
  (published/observed SHA-256
  `d7e18b2597ae8f242f5f31ee9e90deef48dbc9edd634d9868fb6435d08c07f02`),
  extracted binary SHA-256
  `3188814c35471432d4123203e0eb38e5bddc60226e3d7ddf0e59e649ea140022`,
  and version `codex-cli 0.154.0`. The protected launch and second-client native
  pagination bootstrap passed with the requested path still a direct socket.
- `TestSandboxedAppServerLaunch` passed against that binary: protected server
  initialization plus a second external client's initialize/paginated resume of
  the same empty thread.
- `TestCompatCodex01571ProtectedAttachReconnect` passed with `--unshare-net`:
  protected server initialize, protected second-namespace native bootstrap,
  deliberate daemon-side transport loss, two concurrent reconnect admissions,
  and unchanged supervisor/PID/thread. No credentials, account operation, model
  turn, or external network was available.
- `TestScopeMapsOnlyOwnCodexPhysicalSocket` passed real bubblewrap probes for own
  access and sibling/hash/host-source/symlink-alias denial.
- Focused unit packages passed for exact symlink resolution, old direct sockets,
  redirected/malformed alias denial, and deterministic two-listener replacement
  races for both direct and relocated endpoints. The host reached the original
  pinned socket after the source leaf was replaced; it never reached the peer.
- Ubuntu namespace CI now runs the exact physical-boundary probe, the existing
  sandbox launch against pinned 0.153.4, and the digest-pinned 0.157.1 protected
  initialize/native-bootstrap/reconnect fixture.
