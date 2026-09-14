# Session dispatch reliability fix

Base: `0c0e7d489f3f48d7aa9d420cf005186f31738daf`

## Reproducers and causes

All regressions use temporary files, fake engines, or in-memory App Server
transports. They do not start a real App Server or model task.

1. `TestSessionDispatchAcceptedPromptOutlivesCallbackContext` reproduces a
   session-mailbox prompt returning `ok` before its deferred cold start acquires
   final admission. `dispatchCallback` then cancelled its callback-local context,
   so the accepted start failed with `context canceled`.
2. `TestStructuredPromptRebindsCallbackCancellationToServingLifetime` covers the
   same boundary through bounded `BeginPrompt` admission. Deferred work now keeps
   the authenticated guard, revalidates it under `dispatchMu`/`effectMu`, survives
   only the completed callback timeout, and remains cancellable by the daemon's
   serving lifetime.
3. `TestManagerDoesNotKeepDisconnectedSupervisorLiveWithPendingTool` closes an
   in-memory transport after a `tool_call` without a matching result. Pending tool
   history is not liveness: `Manager.Get` and `Manager.Live` now retire the closed
   RPC handle and its process instead of publishing it as ready.
4. `TestForeignThreadStatusAndStartDoNotContaminateSupervisor` sends auxiliary
   review-thread `thread/started` and `thread/status/changed` notifications while
   the primary turn is active. Previously the former overwrote the pinned thread,
   allowing an interject to combine an auxiliary `threadId` with the primary
   `expectedTurnId`. Thread-scoped foreign notifications are now discarded, the
   handshake response remains the sole identity source, and interject retains the
   primary pair.
5. `TestSessionEventPagerEOFResponseDoesNotHideLaterAppend` reads through EOF,
   polls once unchanged, appends one normalized event, then polls the returned
   cursor. An unchanged deterministic successor used to memoize the empty JSON
   page on itself forever. Unadvanced EOF pages are now recomputed; pages that
   consume state still keep exact same-cursor retry behavior.

READY/IDLE reports and model display are separate from App Server notification
filtering. Rail activity comes from the last subject/runtime-scoped hook report
and is gated by manager liveness; tool events do not rewrite it. Snapshot model
reconciliation comes from `Harness.CurrentModel` (an exact runtime report or
rollout), then calls `Supervisor.SetModel`; `onNotify` does not change the model.
Before a Codex UUID is adopted, the existing private-home newest-rollout fallback
cannot distinguish an auxiliary reviewer rollout. That is a separate telemetry
attribution limitation requiring primary-thread identity in the report/rollout
selection contract; this focused lifecycle patch does not change the configured
primary model or suppress automatic review activity.

## Diff

- Rebind accepted asynchronous steer work from the bounded session-RPC callback
  to its serving lifetime without dropping context values or policy checks.
- Make authorization guards evaluate against the final-admission context.
- Treat a closed supervisor RPC transport as dead in manager routing/liveness.
- Filter all explicitly foreign thread/turn/item notifications, including nested
  `thread.id`, and stop notifications from assigning durable identity.
- Avoid self-caching an unadvanced EOF event page.

## Verification

- `go test ./internal/daemon ./internal/codexapp -run '<five regressions>' -count=10`
- `go test ./internal/daemon ./internal/codexapp -count=1`
- `go test -race ./internal/daemon ./internal/codexapp -count=1`
- Full repository checks and CI: pending while the draft PR is opened.

## PR and operational follow-up

Draft PR: <https://github.com/Kchymet/agent-multiplexer/pull/148> from
`amux/c22e02-d5885a`.

No daemon install or restart was performed. After merge, the release owner must
install the merged binary and restart the daemon before existing sessions can use
the corrected dispatcher/supervisor lifecycle. Existing failed sessions are not
silently replayed; their prompt must be explicitly retried after deployment.
