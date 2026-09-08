# Scoped runtime-event pages

Status: source/API checkpoint for coordinator and lifecycle-owner review. This
does not authorize shared dispatcher or policy edits, deployment, or a readiness
claim.

## Boundary

`runtime-events` is a read-only restricted query. The signed request names only
an authoritative session ID plus pagination fields. It never accepts a runtime,
conversation UUID, source path, transcript path, or filesystem root.

The dispatcher must authenticate and authorize the exact canonical request
before calling the pager. The pager then reopens the read-only store, requires
the target to still be a tracked row, and derives its source from that row using
daemon-owned runtime-record logic with the untracked `ListSessions`/UUID fallback
disabled. A missing, deleted, foreign, or removed-member target fails closed.
The current caller must be unarchived. An ordinary worker can read only itself;
a repo-home can read only its authoritative repo descendants; a coordinator can
read itself and current direct members, including archived members that still
retain that coordinator as `RootID`. This history exception grants no start,
steer, restore, PTY, or other lifecycle authority.

Source files are opened only from that daemon-derived record. Opened handles
must be regular files and must still name the derived path; symlink, non-regular,
and changed/replaced handles are rejected before any bytes are decoded. The
reader never calls the existing whole-history replay helpers and never reads a
caller-selected path.

## Request and response

The restricted request is:

```text
route=query, verb=runtime-events, id=<tracked session id>
fields: cursor=<opaque token> | after_sequence=<non-negative decimal>
```

`cursor` and `after_sequence` are mutually exclusive. Omission starts before
sequence 1. Unknown/duplicate representations, negative/overflowing sequence
values, malformed or stale cursors, and a cursor bound to another target are
invalid requests. `after_sequence` is accepted only as a bounded initial scan
hint: the pager advances through a bounded amount of source per call and returns
a continuation cursor even when no event was produced. A cursor binds the target,
runtime/source layout, source file identities, byte-boundary anchors, decoder
state, and last assigned sequence. Append-only growth preserves it; truncation,
replacement, source-layout/runtime change, or daemon restart invalidates it
explicitly.

The normalized response is `core.RuntimeEventPage`:

```json
{
  "target": "agent-id",
  "runtime": "codex",
  "after_sequence": 12,
  "events": [{"sequence": 13, "event": {"type": "text"}}],
  "next_cursor": "opaque",
  "has_more": true,
  "truncated": false,
  "oversized": {"sequence": 14, "type": "raw", "encoded_bytes": 90000}
}
```

`events` are the existing `harnessproto.RuntimeEvent` normalization, each with
an explicit sequence. The JSON encoding of the complete page, including its
wrapper and cursor, must be at most `sessionrpc.MaxResponseBody` (64 KiB).
Events are added only after measuring the whole candidate page. If the next event
does not fit, it remains for the next cursor. If one event cannot fit in an empty
page, the response advances past it and reports deterministic `oversized`
metadata (`sequence`, normalized `type`, exact encoded byte count); no oversized
payload is partially returned or silently skipped. `truncated` is true for that
page. A source record exceeding the decoder line cap is handled the same way and
discarded in bounded chunks through an explicit cursor state.

Each request is capped by source bytes, complete records, normalized events, and
wall/context cancellation checks. Hitting a work cap returns a valid empty or
partial page with `has_more=true`; it is not permission to allocate or scan the
rest of a transcript. Cursor state/cache admission and expiry are bounded; an
evicted cursor returns `cursor_invalid` rather than replaying unbounded history.

## CLI and ownership split

Ordinary sandbox spelling:

```text
amux agent events [<session-id>] [--after <sequence> | --cursor <token>] [--json]
```

With no ID, the command obtains the caller subject from the fixed authenticated
session context and requests its own page. A coordinator supplies a member ID,
including an archived member still in its workgroup. Text output prints events
and a copyable `next cursor: ...`; `--json` emits exactly one page. It never
opens the store or transcript locally and does not auto-start the daemon.

This worker owns the page types, pager implementation in
`internal/daemon/session_events.go`, synthetic pager/file-RPC tests, the CLI
command/client query helper, and focused generated guidance. The lifecycle owner
owns the minimal integration glue:

1. canonical query validation must admit only `cursor` and `after_sequence`;
2. canonicalization must preserve those fields into `core.Action.Fields`;
3. policy must allow archived *targets* only for the read exception above while
   continuing to deny archived callers;
4. `restrictedQuery` calls the pager only after its existing execution-boundary
   reauthorization and maps invalid/stale cursor errors to stable restricted RPC
   codes without exposing host paths.

The hook/report owner retains authenticated report routes. The permission
producer retains occurrence binding and live-generation authority. The pager
consumes normalized mappings and daemon-proved bindings but creates neither.
