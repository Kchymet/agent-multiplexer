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
  the legacy unmarked database and migrates to schema one. Each daemon reports
  the schema range it can migrate/open and refuses a database outside that range
  before applying migrations.

This keeps compatibility decisions attached to the contracts that matter. A
patch release does not become incompatible merely because its label differs,
while a protocol or schema break cannot hide behind an unchanged executable
name.

## Upgrade behavior

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
