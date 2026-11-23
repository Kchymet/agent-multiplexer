# Branch names and broker id

Every agent amux creates commits on its own branch:

```
amux/<broker>/<workgroup>-<session>      e.g. amux/devbox/50ee30-5a3bab
```

`core.BranchFor` is the single source of this scheme. The branch is stored on
the session when it is created, so everything downstream — the worktree, the
agent's CLAUDE.md, the PR watcher, the archive/rm open-PR guard and
`amux doctor` — reads the stored branch rather than recomputing it.

## The broker segment

A *broker* is the amux daemon/host that owns a set of sessions. Several machines
can run amux against the same GitHub repositories; the broker segment keeps
their branches from colliding and lets each broker's PR watcher claim only its
own PRs (see [PR watch](pr-watch.md)).

The broker id is resolved once per daemon process:

1. `AMUX_BROKER_ID`, when set, wins for that process (it is not persisted).
2. Otherwise the id saved in `<data dir>/broker-id`
   (`~/.local/share/amux/broker-id`, or `$XDG_DATA_HOME/amux/broker-id`).
3. Otherwise amux derives one from the short hostname (`devbox.local` →
   `devbox`; a random `broker-<hex>` when the hostname is empty or
   `localhost`) and saves it to that file, so a later hostname change does not
   change it.

To rename a broker, edit `broker-id` (or set `AMUX_BROKER_ID`) and restart the
daemon. Only sessions created afterwards use the new name; existing sessions
keep the branch they were created with. Give each machine a distinct id — two
brokers with the same id share a namespace again.

Every value is sanitized to a single git ref component: lower-case `[a-z0-9-]`,
other runs collapsed to one `-`, trimmed, at most 32 characters.

### No collisions with older branches

Git cannot hold both a ref `amux/X` and refs under `amux/X/` (a file/directory
conflict, locally and when pushing to GitHub). Pre-broker branches are
`amux/<root>` and `amux/<root>-<agent>[-suffix]`, where `<root>` is a
six-hex-digit session id. A broker id that starts with six hex digits followed
by `-` or the end (`abc123`, `abc123-mini`) is therefore prefixed with `b-`
(`b-abc123`), which makes `amux/<broker>/…` disjoint from every older branch.
The root and session are still joined by `-`, not `/`, for the same reason.

## Older schemes (still supported)

| Scheme | Example | Where it comes from |
| --- | --- | --- |
| `amux/<broker>/<wg>-<session>` | `amux/devbox/50ee30-5a3bab` | current |
| `amux/<wg>-<session>` | `amux/50ee30-5a3bab` | sessions created before the broker segment |
| `amux/<root>` | `amux/50ee30` | pre-hierarchy workspaces |

Sessions on an older scheme keep working unchanged: their worktrees stay on the
stored branch, the PR watcher and open-PR guard match their stored branch, and
`core.ParseBranch` / `amux doctor` decode all three. An agent may push sibling
branches named `<branch>-<suffix>`; those belong to the same session.
