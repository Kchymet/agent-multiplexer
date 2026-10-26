# Session RPC scheduling

How the daemon decides when to serve a session's mailbox request, and in what
order. The mailbox format and its authority model are unchanged by any of
this: see `internal/sessionrpc` and [session mailbox
pressure](session-mailbox-pressure.md).

## Wakeups

A request is a file renamed into the subject's `requests/` directory; its
response is a file renamed into `responses/`.

- The daemon keeps one process-wide inotify instance (Linux) or kqueue
  (macOS). A rename into a watched `requests/` directory wakes the serving
  loop for that subject, which starts a pass for it at once. A pass that was
  already running when the request landed is rerun when it ends.
- The client watches its `responses/` directory, and the mailbox directory
  for a restart's `service.json` replacement, while it waits.
- Events are hints. Every consumer still lists and reads through the
  descriptors it already holds, so a missed, late or spurious event only costs
  latency. A 1 s fallback (`sessionrpc.WatchFallbackInterval`) covers missed
  events. A mailbox whose directory cannot be watched keeps the old polling
  intervals: 50 ms in the daemon, 20 ms in the client.
- Reconcile still runs every 50 ms to open, renew and retire mailboxes.

## Lanes

Within one mailbox, calls are split by `sessionCallSlow`
(`internal/daemon/session_lanes.go`):

| lane | calls | dispatched |
| --- | --- | --- |
| fast | report context; activity, model, limits and permission reports; version, sessions, repos, snapshot and other store queries; central credential; native credential broker | inline by the pass that claims them, in arrival order |
| slow | actions (create, start, restart, archive, steer, rename, git and worktree work, and any unknown verb); agent-session discovery; runtime-event pages; transcript capture | by one worker per mailbox, in arrival order, at most `sessionrpc.MaxSlowRequests` claimed at once |

Receipts and releases are transport records and are always handled inline.

The server peeks at the call before authenticating it, only to pick a lane;
the worker authenticates, authorizes and dispatches exactly as before. Slow
requests beyond the lane's bound stay claimed in `processing/` and are taken
when it has room. Closing a mailbox joins its slow lane, so the daemon hands
that close to a goroutine, as it already does for a busy pass.

## Ordering

- Fast calls keep their arrival order among themselves: successive activity
  reports, a permission request before its resolution.
- Slow calls keep their arrival order among themselves: two actions from one
  session run one after the other.
- A fast call can finish before a slow call that arrived earlier. A single
  client waits for each response before sending its next call, so only
  concurrent callers in one session, which have no order to keep, can see
  this.
- Mailboxes of different subjects never wait on each other.
