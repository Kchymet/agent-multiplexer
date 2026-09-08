# Scoped runtime-event reads

Status: staged for integration. The command described here becomes usable only
when the lifecycle owner's authenticated server policy/dispatch glue lands; the
scoped pager/CLI checkpoint alone does not make `runtime-events` available.

Agent sandboxes read history through authenticated daemon queries. They do not
need—and are not granted—sibling sandbox, state-directory, or transcript paths.

Read one page for the calling agent:

```sh
amux agent events --json
```

A workgroup coordinator can name itself or one of its authoritative direct
members. An archived member remains readable while it still belongs to that
workgroup:

```sh
amux agent events <member-id> --json
amux agent events <member-id> --cursor <next-cursor> --json
```

Use `amux status --json` for the scope-filtered session inventory. Ordinary
workers are self-only; repo-home and coordinator reads are checked against the
daemon's current authoritative membership. Deleted, removed, foreign, and
untracked conversation IDs are denied. Read access does not grant PTY, start,
restore, steering, or other lifecycle authority.

The dispatcher checks scope before source I/O and again immediately before it
releases the page. Revocation or membership change during the bounded read thus
drops the protected response; file I/O does not run while holding the global
effect mutex.

Each response—including its wrapper—is at most 64 KiB and carries an opaque
continuation cursor. A cursor expires after a bounded idle period and is
invalidated by source replacement, truncation, runtime/source-layout changes,
or daemon restart. Append-only growth remains readable. `--after <sequence>` is
an initial skip boundary and is mutually exclusive with `--cursor`; because
scanning is bounded, catching up to a large sequence can require empty
continuation pages.

The sequence belongs to this cursor chain. It is deterministic across that
chain's fixed source order, but it is not a provider-stream resume sequence:
independently started readers can observe later appends to multiple sources in a
different order.

An event too large for an otherwise empty response is omitted with explicit
normalized-event metadata. A raw source record larger than the decoder limit is
discarded in bounded chunks and reported separately with only its raw byte size
and reason; amux does not claim a normalized type or encoded size for content it
did not parse.
