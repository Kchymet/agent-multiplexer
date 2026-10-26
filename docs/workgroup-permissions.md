# Workgroup permissions: host, coordinator, members

A one-screen map of who may do what inside a workgroup. It describes the
daemon's enforced policy as it stands in `internal/access/scope.go` and
`internal/access/policy.go`; the per-operation table in
[Agent CLI access and authorization](agent-cli-sandbox.md#control-api-scopes)
is the canonical list and is not repeated here. The surrounding roles are in
[Default sessions](default-sessions.md), and the wider system in the
[architecture overview](architecture-overview.md).

## Hierarchy

```
HOST OPERATOR  (TUI, host CLI, web)                     never restricted
  creates workgroups · sets the coordinator's repo grants · restores
  archived sessions · changes goal state · moves, deletes, kills
  │
  ▼  one workgroup; its root row is the coordinator
┌─────────────────────────────────────────────────────────────────────┐
│ WORKGROUP A — COORDINATOR  (role coordinator, id = workgroup id)     │
│  scope: itself + every session whose current RootID is this id      │
│  may: add-agent (own workgroup, granted repos only)                  │
│       read member events · start · rename · steer · archive members │
│       answer a MEMBER's permission request (never its own)          │
│       archive the whole workgroup — after explicit user confirmation│
│  may not: restore · touch another workgroup · change repo grants    │
│           · steer goal · move/delete/kill · open panes or raw paths │
│      │ control flows one way: coordinator → member                   │
│      ▼                                                               │
│  ┌───────────────────────────┐   ┌───────────────────────────┐      │
│  │ MEMBER A  (role empty)    │   │ MEMBER B  (role empty)    │      │
│  │ self only: own events,   │   │ self only                 │      │
│  │ status, name, start/stop,│   │ no sibling read, no       │      │
│  │ prompt, `amux agent done`│   │ sibling control, no       │      │
│  │                          │   │ coordinator control       │      │
│  └───────────────────────────┘   └───────────────────────────┘      │
└─────────────────────────────────────────────────────────────────────┘
```

Three things that look like access but are not session control:

- **Discovery is metadata, not access.** `amux agent sessions` lists every
  host conversation's harness, path and cwd for any active session. It is the
  deliberate read-only cross-session exception; it relaxes no event read and
  no action (`internal/daemon/session_discovery.go`). Reading a session's
  history is `amux agent events <id>`, which is workgroup-scoped.
- **A control grant is not filesystem access.** A coordinator can steer and
  read a member through the authenticated daemon RPC, but its sandbox mounts
  only its own `coordinator/` directory, never the members' worktrees
  ([sandbox configuration](sandbox-config.md)). Members likewise see only
  their own worktree and private harness config.
- **Nobody approves itself.** A permission answer is accepted only from a
  principal other than the prompting session; see below.

## Enforced details

All of these are checked on the caller's signed session credential. Request
ids, fields and environment variables only ever name a target
(`policy.go`, `Authorize`).

- **Scope is self, or self plus current direct members.** `InWorkgroupScope`
  returns true for a coordinator when the target is itself or the target's
  `RootID` equals the coordinator's id. Ordinary agents and repo homes match
  only themselves. Membership is the target's live daemon-owned `RootID`, so a
  member moved to another workgroup leaves the old coordinator's scope at once
  and joins the new coordinator's. Policy runs when the request is claimed,
  again under the dispatch lock immediately before the action executes, and
  again before an event page is released.
- **Archived members: reads yes, effects no.** A coordinator may still page
  an archived member's events while the member's `RootID` points at it; every
  action on an archived target is denied. A member whose parent workgroup is
  archived is treated as archived for authorization even if its own flag is
  clear. An archived caller has no control access at all.
- **Permission answers need proof of the prompt.** `steer verb=permission`
  must name `request_id` and `runtime_generation`, both taken from the
  member's `permission_request` event, and is never accepted on the caller's
  own id. The daemon then refuses an id that does not match an open request of
  the current runtime generation, so a replayed or guessed answer cannot land.
- **add-agent stays inside the ceiling.** A coordinator can add only to its
  own workgroup, with the fields `agent`, `prompt`, `mode`, `model`, `repos`,
  and every listed repo must be in the grant set the host stored for that
  coordinator. The ceiling itself (`coordinator-set-repos`) is host-only.
- **Self-completion.** `amux agent done` resolves the caller from the fixed
  session context and sends `set-archived archived=true` on that id. For a
  member this archives only the member. For a coordinator it archives the
  root, and direct members inherit the archived state: runtimes stop and
  credentials are revoked. Repo homes and the console never archive
  themselves.
- **Host-only, for every session principal.** Restore (`archived=false` and
  the `archive` toggle), `steer verb=goal`, repo-grant changes, move, delete,
  kill, `new-workgroup` (except a repo home creating one limited to its own
  repo), pane access, and raw runtime paths or records.

## Group archive: an instruction, not a daemon check

The coordinator's guide, the create-pr skill and the goal-mode docs all say a
coordinator archives its workgroup only after explicit user confirmation, and
that a completed goal is not confirmation. That requirement is an agent
instruction. The daemon cannot observe the conversation: `allowArchive` in
`scope.go` accepts a coordinator's `set-archived archived=true` on its own id
on role alone, and `amux agent done` carries no confirmation field. Treat the
confirmation as a behavioral contract the coordinator is bound to, enforced by
its instructions and reviewable in its transcript, not by policy. The host can
always restore a workgroup archived early (`amux workgroup unarchive <id>`).

## Console and repo homes

The **console** is the operator's delegated view: it passes any rule marked
`Console` on any target, and is still refused pure host operations such as
auth reload or restart. A **repo home** is self-only like a member, cannot
archive itself, and holds one host-scope exception: it may create a new
workgroup limited to exactly its own repo. Sharing a repo with a workgroup
grants a repo home nothing inside that workgroup. Details are in
[Default sessions](default-sessions.md).
