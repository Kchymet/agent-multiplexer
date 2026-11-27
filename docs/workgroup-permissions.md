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
AUTHENTICATED HOST OPERATOR  (local TUI, host CLI)      never restricted
  creates workgroups · sets the coordinator's repo grants · restores
  archived sessions · steers goal state · moves, deletes, kills
  (remote/provider callers hold their own separate grant)
  │
  ▼  one workgroup; its root row is the coordinator
┌──────────────────────────────────────────────────────────────────────┐
│ WORKGROUP A — COORDINATOR  (role coordinator, id = workgroup id)     │
│  scope: itself + every session whose current RootID is this id       │
│  may: add-agent (own workgroup, granted repos only)                  │
│       read member events · start · rename · steer · archive members  │
│       answer a MEMBER's permission request (never its own)           │
│       archive the whole workgroup — when wrap-up rules allow (below) │
│       triage items members raise · escalate to the global inbox      │
│  may not: restore · touch another workgroup · change repo grants     │
│           · steer goal · move/delete/kill · open panes or raw paths  │
│      │ control flows one way: coordinator → member                   │
│      │ items flow the other way: member → coordinator → global inbox │
│      ▼                                                               │
│ ┌────────────────────────────┐   ┌────────────────────────────┐      │
│ │ MEMBER A  (role empty)     │   │ MEMBER B  (role empty)     │      │
│ │ self only: own events,     │   │ self only                  │      │
│ │ status, name, start/stop,  │   │ no sibling read, no        │      │
│ │ prompt, `amux agent done`, │   │ sibling control, no        │      │
│ │ `amux agent raise` (items) │   │ coordinator control        │      │
│ └────────────────────────────┘   └────────────────────────────┘      │
└──────────────────────────────────────────────────────────────────────┘
```

Three things that look like access but are not session control:

- **Discovery is metadata, not access.** `amux agent sessions` can expose
  cross-session metadata (harness, transcript path, cwd) to any active
  session. It is the deliberate read-only cross-session exception; it relaxes
  no event read and no action (`internal/daemon/session_discovery.go`).
  Reading a session's history is `amux agent events <id>`, which is
  workgroup-scoped.
- **A control grant is not a filesystem grant.** What a session may do over
  the authenticated daemon RPC and what its process can see on disk are
  separate grants. A coordinator can steer and read a member through the RPC
  while its sandbox does not mount the member-containing workgroup parent;
  the mount set itself is described in
  [sandbox configuration](sandbox-config.md) and [Security](../SECURITY.md).
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
  own id. The daemon then accepts the answer only if the request id names a
  prompt the runtime currently has open and the generation names the current
  runtime; otherwise it is refused (`internal/daemon/steer.go`,
  `checkPermissionRequest`).
- **add-agent stays inside the ceiling.** A coordinator can add only to its
  own workgroup, with the fields `agent`, `prompt`, `mode`, `model`, `repos`,
  and every listed repo must be in the grant set stored for that coordinator:
  either a fixed list (the workgroup was created naming repos, or the host set
  one) or, by default, every repo currently tracked on the host. Either way it
  never reaches an untracked repo. Visibility is separate from authority:
  `amux repo ls` always shows a coordinator every tracked repo, marking which
  are granted, but add-agent on an ungranted one is denied (`repo_not_granted`,
  naming the repo and `amux repo grant`). The host adjusts an existing
  workgroup at any time with `amux repo grant <id> [--all | --none |
  repo...]` (`coordinator-set-repos`, `all=1` for all tracked); the change
  applies to the next add-agent, move or agent-set-repos without a restart.
  Changing grants and tracking or untracking repos are host-only.
- **Self-completion.** `amux agent done` resolves the caller from the fixed
  session context and sends `set-archived archived=true` on that id. For a
  member this archives only the member. For a coordinator it archives the
  root, and direct members inherit the archived state: runtimes stop and
  credentials are revoked. A repo home cannot archive itself. The dispatch
  path accepts a self-completion only from an agent or coordinator role
  (`internal/daemon/session_dispatch.go`, `isSelfCompletion`).
- **Items flow upward, acted on as oneself.** `raise-item`, `item-state`,
  `escalate-item` and query `items` always name the caller as their target
  (`allowSelfItem`); no session acts on items as another — not even a
  coordinator for its member, nor the console for anyone. Routing is decided
  by the daemon, never the caller: a member's item goes to its own
  coordinator, every other session's to the global inbox (`access.ItemRecipient`;
  its owner is resolved once, in `items.GlobalInbox()` — the overseer).
  Per item (`internal/access/items.go`): the source and recipient see it, a
  coordinator also sees its workgroup's items, the global inbox owner and the
  admin console see all; the recipient (or those two) triages it, the source may only withdraw it;
  only a coordinator holding an item may escalate it, and only to the
  global inbox. Archiving or deleting a recipient carries its active items to
  the global inbox in the same transaction. See [Items](items.md).
- **Unavailable to every session principal.** Restore (`archived=false` and
  the `archive` toggle, refused for every restricted caller including the
  console in `internal/daemon/session_operation.go` before policy runs),
  `steer verb=goal`, the coordinator repo ceiling
  (`coordinator-set-repos`), daemon restart, auth reload, account and login
  management, `subscribe`, pane access, and raw runtime paths, records and
  account queries. Other host-scope operations (move, delete, kill, repo
  add/remove, `new-workgroup`, `create-workspace`, refresh) are delegated to
  the console and otherwise denied; the
  [canonical table](agent-cli-sandbox.md#control-api-scopes) marks each one.
- **Goal steering is not goal reporting.** `steer verb=goal` is the host's
  control over a goal session's native goal: `status` active, paused,
  complete or clear, an optional replacement objective, and an optional token
  budget. It is distinct from the runtime reporting its own goal progress
  (Codex `update_goal`, or `amux agent goal …` on a Claude coordinator),
  which a coordinator does for itself and which never pauses, resumes or
  clears the goal from outside.
- **Authorization ceiling versus runtime support.** The policy decides
  whether a principal may send an operation; the runtime decides whether it
  can be carried out. An in-scope `interject`, `stop` or permission answer
  still fails when the member has no live turn, no open prompt, or a runtime
  that cannot prove its open requests. A denial is a scope result; an
  execution failure is not a grant.

## Group archive: an instruction, not a daemon check

The coordinator's guide ("Wrapping up this workgroup"), its goal hooks and the
create-pr skill say when a finished coordinator archives its own workgroup: on
its own only when every PR is verified MERGED on GitHub and nothing is pending
(no follow-up, open decision, running member or unanswered user message), and
otherwise only after asking the user (Claude Code: `AskUserQuestion`; Codex: a
plain-text question). That requirement is an agent instruction.
The daemon cannot observe the conversation: `allowArchive` in `scope.go` accepts a coordinator's `set-archived archived=true` on its own id
on role alone, and `amux agent done` carries no confirmation field. What the
daemon does enforce: a coordinator can never force its own archive past an open
member PR, and the archive answers the caller before its runtime is stopped
(once the turn ends, or after a short grace). Treat the wrap-up rules as a
behavioral contract the coordinator is bound to, enforced by
its instructions and reviewable in its transcript, not by policy. The host can
always restore a workgroup archived early (`amux unarchive <id>`).

The same holds for open items: the guide has a coordinator triage its inbox,
auto-archive only with nothing left open there, and otherwise ask, naming the
open items. What the
daemon does enforce is that none is lost — the archive transaction moves every
active item in the coordinator's inbox to the global inbox.

## Console, overseer and repo homes

The **admin console** is the operator's delegated view: it passes any rule marked
`Console` on any target, and is still refused pure host operations such as
auth reload or restart. It keeps an all-items operator view, but nothing
escalates to it, and it is never reachable over direct access
([direct-console.md](direct-console.md)).

The **overseer** (role `overseer`) is the coordinator of coordinators and the
global inbox owner (`items.GlobalInbox()`). It holds exactly the rules marked
`Overseer` in `internal/access/scope.go`, narrowed per verb by `allowOverseer`
(`internal/access/overseer.go`); there is no blanket shortcut, so every unmarked
rule is denied to it whatever its scope. Its scope is every session except the
console. `TestOverseerAccessMatrix` pins each cell:

| Operation | Overseer | Admin console |
| --- | --- | --- |
| read sessions, snapshot, repos, events, `wait` | every session but the console | everything |
| query `queue` (the user question queue) | read | read |
| `start` | coordinator rows only (starts the workgroup) | yes |
| `steer` prompt / interject / stop | coordinators only; members go through their coordinator | yes |
| `steer` permission, goal | never (only the user answers, through the queue) | permission yes |
| `set-archived` | `archived=true`, never `force`, a workgroup root or a member | yes |
| `new-workgroup` | only `-f from_item=<item>` naming an item the **host** acknowledged; tracked repos only | yes |
| `raise-item`, `item-state`, query `items` | as itself (it sees and triages every item) | as itself |
| `refresh` | yes | yes |
| `rename`, `add-agent`, `escalate-item`, `move`, `delete`, `kill`, `add-repo`, `rm-repo`, `agent-set-repos`, `create-workspace` | never | yes (escalate: never) |
| `queue-answer`, `queue-dismiss`, and every host-only operation | never | never |

No coordinator, member or repo home can steer the overseer; they reach it by
raising items. When the overseer needs an admin action it raises a
`decision_needed` item naming the command for the user to run at the admin
console — never escalation by proxy. The host-acknowledgment rule makes a
prompt-injected overseer unable to spawn work on its own say-so
(`access.ItemApprover`, `itemsvc.HostAcknowledged`).

A **repo home** is self-only like a member, cannot
archive itself, and holds one host-scope exception: it may create a new
workgroup limited to exactly its own repo. Sharing a repo with a workgroup
grants a repo home nothing inside that workgroup. Details are in
[Default sessions](default-sessions.md).
