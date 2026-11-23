# Items: upward signals

When a session finishes a turn or its work, the things someone upstream should
act on or know — a follow-up, outstanding work, a decision it needs, a risk, or
something it deliberately left out of scope — travel as **structured items**,
not as prose buried in a final message. Items are raised by sessions, triaged
by coordinators, and end up in the **global inbox** when they need the user.

## Flow

```
member ──raise──▶ its coordinator ──escalate──▶ global inbox
coordinator, repo home, agent outside a workgroup ──raise──▶ global inbox
coordinator archived/deleted ──carry-over──▶ global inbox
```

- A **member** of a coordinated workgroup raises to its coordinator.
- A **coordinator** triages what it receives: it resolves what it can
  (`done`, `ack`, `dismiss`) and **escalates** to the global inbox what needs
  the user or crosses workgroups. What a coordinator raises itself goes to the
  global inbox directly — so do the items of repo homes, agents outside a
  coordinated workgroup, and the owner's own.
- The **global inbox** spans every workgroup. Its owner is one session,
  resolved in exactly one place (`items.GlobalInbox()`); today that is the
  console session (`console`), and it can be repointed there without touching
  routing, authorization, carry-over or delivery. The owner — and the admin
  console — see every item.

## Item

| Field | Meaning |
| --- | --- |
| `id` | `it-` + 8 hex digits |
| `kind` | `follow_up` · `outstanding` · `decision_needed` · `risk` · `fyi` |
| `title`, `detail` | one line (≤ 200 chars); free text (≤ 4000 chars) |
| `prs`, `links` | `owner/repo#n` or `repo#n`; http(s) URLs (≤ 10 each) |
| `suggested_action` | what the source recommends doing (≤ 1000 chars) |
| `state` | `open` · `acknowledged` (active) · `dismissed` · `done` (closed) |
| `source`, `workgroup` | the raising session; its coordinator when raised |
| `recipient` | whose inbox it is in now: a coordinator id or the global inbox owner (`items.GlobalInbox()`, today `console`) |
| `raised` | how many times the source raised it (de-duplicated raises) |
| `created`, `updated`, `closed` | Unix milliseconds |
| `history` | every raise, update, state change, escalation and carry-over: who, from → to, note |
| `unnotified` | the recipient has not been woken for the latest change |

**De-duplication.** Raising again with the same source, kind and normalized
title (case-folded, whitespace collapsed, surrounding punctuation trimmed)
while that item is active updates it — detail and suggestion replaced, PRs and
links merged, `raised` incremented, the current recipient re-notified —
instead of creating a duplicate. A closed item no longer absorbs raises. One
session may hold at most 100 active items.

## Commands

All act as the caller; identity comes only from the fixed session context.

```
amux agent raise --kind <kind> --title "…" [--detail "…" | --detail -]
                 [--pr repo#n]... [--link url]... [--suggest "…"] [--json]
amux agent items [--raised | --inbox] [--state open,acknowledged | --all] [--limit n] [--json]
amux agent items show <id> [--json]
amux agent items ack|dismiss|done|reopen <id>... [--note "…"]
amux agent escalate <id>... [--note "…"]          # coordinator → global inbox
```

The host may act as any session through the control socket:
`amux do raise-item <session> -f kind=… -f title=…`,
`amux do item-state console -f item=it-… -f state=done` (acting as today's global inbox owner),
`amux do escalate-item <coordinator> -f item=it-…`. A host `query items`
without an id reads as the global inbox owner (every item).

## Authorization

Every item verb names the acting session as its target (`Action.ID`), and the
route rules in `internal/access/scope.go` admit it only for the caller itself
(`allowSelfItem`; escalation also requires the coordinator role). Which item
the caller may then see or move is decided from stored state in
`internal/access/items.go`:

| | sees | ack / reopen | dismiss / done | escalate |
| --- | --- | --- | --- | --- |
| source | its own items | no | while active (withdraw) | no |
| recipient | its inbox | yes | yes | coordinator only, to the global inbox |
| coordinator | also every item raised in its workgroup, after escalation too | only what it holds | only what it holds | only what it holds |
| global inbox owner, admin console | every item | yes | yes | no (already at the top) |

An item the caller may not see answers `item_not_found`, so its existence
never leaks. Inputs are validated server-side (kind, lengths, control
characters, PR and link shapes). Archived callers have no access.

## Delivery

Items reuse the PR watcher's wake-up queue ([PR watch](pr-watch.md)). Any
change that leaves an item unnotified for its recipient — a raise, a
de-duplicated re-raise, an escalation, a reopen, a carry-over — queues one
`items` wake for that recipient; bursts coalesce in the 3-second window, one
wake per recipient. At delivery the daemon re-reads the recipient's unnotified
items, types one prompt listing them —

```
[amux goal] Items raised (amux wake-up, not a new task):
- item it-1a2b3c4d [follow_up] "Document the --foo flag" (from m1)
Check these items and act on them as your task requires. Triage them with `amux agent items` …
```

— under the same gating as every wake-up (live, between turns, no prompt open,
a continuable goal), and marks exactly those items notified. A coordinator
gets item lines in the same batch as member and PR lines. The global inbox
owner is woken the same way when it is live and idle. An undelivered notice is never lost:
the items stay unnotified, and the next change, a coordinator leaving the rail,
or a daemon restart offers them again.

## Archive carry-over

Archiving a session (or deleting it) moves every **active** item whose
recipient it is to the global inbox **in the same store transaction**
(`store.SetArchivedFlag`, `store.DeleteSession`), reopening it there and
recording a `carried_over` history entry. In practice that is a coordinator's
inbox: members never receive items, and a member's raised items stay with its
live coordinator for triage when the member is archived. When the archived
coordinator leaves the rail, the daemon offers the global inbox owner the carried items.

A coordinator does not archive itself to hand items off silently: its guide
has it triage its inbox first, and if items are still open, ask the user
(naming them) before `amux agent done` — which then hands them to the global inbox.

## For implementers

- `internal/items` — kinds, states, validation, de-dup key, transitions.
- `internal/store/items.go` — the additive `items` table (JSON document plus
  indexed columns; no schema version bump), raise/de-dup, filters,
  carry-over, notification marks.
- `internal/itemsvc` — the internal API every surface uses:
  `Raise`, `Get`, `List(Query{States, AllStates, Box, Limit})`, `Transition`,
  `Escalate`, `UnnotifiedRecipients`, `Unnotified`, `MarkNotified`. Every
  method takes the acting session id and applies the access rules.
- `internal/daemon/items.go` — session RPC and host handlers, the bounded
  list response, wake rendering and delivery to the global inbox owner.
