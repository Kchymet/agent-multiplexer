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

## A direct socket fast path: assessment

Could sandboxed sessions skip the file mailbox for fast calls and talk to the
daemon over a socket instead? It is feasible without weakening the access
model, but it is not worth building now. Measured on this tree, the mailbox
is no longer where the time goes.

**Feasible.** There is precedent: the console already reaches the daemon over
a Unix socket inside its own mailbox directory (`ConsoleAccountSocketName`).
Seatbelt grants exactly that one literal socket, and a read-only bind mount
still permits `connect`. A session socket would carry the same signed
envelope as a mailbox request. The daemon would run the same `Verify`,
replay check, `Authorize` and dispatch, so the socket is transport only:
authority, grants and per-subject identity would be unchanged. It must never
trust peer credentials or socket location in place of the signature.

**What it would save.** Per call, the mailbox costs a few file creations,
renames and fsyncs: the client's request publication, the claim, the
response publication and the release. A socket would remove those. After the
wakeup and lane changes an idle trivial call takes about 2 ms p50 end to end.
A CPU profile of a tight idle loop shows `FileAuthority.Verify` at 57% of the
time, with the mailbox's own file work a much smaller share. A socket keeps
`Verify`, so it could save roughly the remaining file share, around 1 ms.

**What it would cost.**

- Durability. The mailbox survives a daemon restart: claimed requests resume,
  committed responses are adopted, and receipts settle. A dropped socket
  leaves the outcome indeterminate, so only the fast, idempotent lane (reports
  and reads) could use it, and every client would need the mailbox as its
  fallback anyway.
- A second transport doubles the audit surface for a protocol whose security
  argument is currently one code path.
- One listener per session, plus Seatbelt rules and reconnect handling across
  daemon restarts.

**Where the time actually goes.** `FileAuthority.Verify` durably records every
accepted request in the replay ledger. It re-marshals the whole ledger, up to
4096 entries, twice (`stateFits`, then the commit) and fsyncs it, while
holding the authority's mutex. That mutex is shared by every session, so under
load all sessions' requests serialize on it. With #206's releases, each call
now verifies twice. That cost is behind the remaining latency under load, and
any socket path would pay it too. Options for the owner of `internal/access`:

1. Keep the durable ledger, but make acceptance O(1): an append-only replay
   journal with periodic compaction, and one marshal instead of two.
2. Decide whether durability beyond the per-start boot nonce is needed.
   `Verify` already rejects every request signed for another boot, and the
   ledger's persistence is defence in depth for the crash window, pinned by
   `TestReplayLedgerSurvivesRestartAndBootNonceChanges`. That trade-off is a
   security decision, not a scheduling one.

Revisit the socket after the ledger cost is gone, if the file work is then
what remains.
