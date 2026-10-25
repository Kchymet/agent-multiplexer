# Versioning and compatibility

`amux version` reports the current CLI build and, without starting anything,
queries the connected local daemon for its build and database schema. `amux
doctor` shows the same information as health checks and exits non-zero for a
known incompatibility.

There are three independent numbers:

- The product version identifies a build. Different CLI and daemon product
  versions are allowed when their protocol remains compatible.
- The daemon protocol identifies the local CLI↔daemon API. Additive queries and
  fields retain the protocol number; a breaking wire or behavior change bumps
  it. Doctor currently accepts an exact match.
- SQLite `PRAGMA user_version` identifies the database schema. Schema zero is
  the legacy unmarked database; the current schema is three. Each daemon reports
  the schema range it can migrate/open and refuses a database outside that range
  before applying migrations.

This keeps compatibility decisions attached to the contracts that matter. A
patch release does not become incompatible merely because its label differs,
while a protocol or schema break cannot hide behind an unchanged executable
name.

## Upgrade behavior

Build with Go 1.26.10 or a newer patched release. The Go standard library is part
of the binary, so updating a Go installation alone does not fix an old amux
executable. Rebuild, reinstall and restart the daemon; reopen dashboards and
restart any provider service. `go version -m /path/to/amux` reports the build's
Go version and, for a Git checkout, revision. `make vuln` checks reachable
vulnerabilities in both modules using the root module's selected toolchain.

Schema three promotes legacy hidden repository workgroups into normal visible
coordinator workgroups. Writable store opening preserves root/member IDs, member
sandboxes, branches, conversation IDs and archive state, adds a dedicated
`coordinator/` directory, and initializes explicit repository grants from the
legacy root's stored repo scope. Repository homes remain management context.
The migration does not move member files or launch runtimes. It is repeatable,
and `user_version` advances to three only when the promotion transaction commits;
a failed promotion from schema two leaves its marker at two so it can be retried.
Read-only store opening never migrates and requires the current schema.

For this upgrade, install the schema-three binary and restart the daemon before
resuming management operations; then reconnect dashboards and restart provider
services. Replacing only the CLI leaves the already-running daemon on its old
code. A schema-two daemon cannot open a database upgraded to schema three and
must be upgraded, not restarted with the older executable. Do not lower
`user_version` to make an older binary accept the promoted data.

The version query is additive. A daemon from before version reporting responds
with an unknown-query error; the CLI reports its version and database schema as
unknown and suggests `amux daemon restart`. It does not claim incompatibility
without evidence. Once both sides report contracts, doctor fails loudly on a
protocol mismatch or on a database outside the daemon's advertised schema
range.

The first schema marker cannot make already-installed, pre-versioning binaries
honor it retroactively. After upgrading across that boundary, restart the daemon
before using the database. All version-aware binaries refuse future schemas,
which prevents an older daemon from silently operating on a layout it does not
understand.

If the daemon protocol later needs rolling compatibility rather than exact
matching, evolve the single protocol number into advertised supported ranges (or
a negotiated version list) while keeping this query shape additive. Database
migrations should remain forward-only, idempotent, and set `user_version` only
after the corresponding migration succeeds.
