# PR watch: member PR state and coordinator wake-ups

amux knows which GitHub pull requests each workgroup member owns, watches each
member's activity, and wakes the member's coordinator when either changes in a
way it should act on. A coordinator that runs `amux agent goal wait` (or simply
ends its turn) while its members work needs no polling loop of its own: amux
pushes the change to it (`internal/prwatch`, wired in
`internal/daemon/prwatch.go` and `internal/daemon/memberwake.go`).

## Ownership

A member owns the PRs whose head branch is its branch (`amux/<broker>/<wg>-<member>`)
or that branch plus a `-suffix` or `/suffix` (`amux/<broker>/<wg>-<member>-b`), in the GitHub
repositories it works in (a tracked repo whose source is a GitHub URL or
`owner/name`; other repos are skipped). Only active members of active
workgroups are watched.

The `<broker>` segment names the amux host that created the member (see
[Branch names and broker id](branches.md)), so a broker only ever claims PRs
from its own branches: another machine running amux against the same repos, even
with colliding workgroup and member ids, pushes under a different
`amux/<other-broker>/` namespace. Members created before the broker segment keep
their stored `amux/<wg>-<member>` branch and are watched exactly as before.

## Polling

The daemon polls on the host with the host user's own `gh` login (agents hold no
GitHub authority for it). Every 3 minutes (`AMUX_PR_POLL_INTERVAL`, a Go
duration such as `90s`, minimum 30s; `off` disables) it runs one
`gh pr list --state open --search head:amux/<broker>/ --json …` per repository —
whatever the number of members (plus one `head:amux/` listing while pre-broker
members remain) — and a single `gh pr view` only for a tracked PR that
left the open list, to learn whether it merged or closed. A missing or
unauthenticated `gh` is logged once and retried every 30 minutes; other
failures (rate limits included) back the interval off exponentially up to 30
minutes. A failed listing never marks a PR closed.

## What is recorded

The last observation per session persists in the store (`session_prs`), so
transitions are reported exactly once across daemon restarts.

- `amux status --json` gives each member row a `prs` list
  (`harnessproto.SessionPR`: repo, number, url, branch, state
  `open|merged|closed`, draft, mergeable, mergeState, checks
  `pass|fail|pending`, failedChecks, reviewDecision, lastReview, updated, and
  additively title, baseBranch, githubUpdatedAt, mergedAt, closedAt, reviews,
  requestedReviewers — all from the same `gh pr list` call). The rail/status
  text gains a short suffix, e.g. `· PR #247 CI✗ conflict`. Merged and closed
  PRs stay listed for 3 days after they ended (at most 10 per session). `prs`
  is kept for older consumers; new ones read `enrichments` (see
  [Session enrichments](session-enrichments.md)).
- With `AMUX_PR_NOTIFY=ready_to_merge,needs_attention` (or `all`; unset means
  none) a transition into "ready to merge" or "needs attention" also sets
  `notify` on its `pr_state` payload, once per entry into that phase and never
  for a PR seen for the first time.
- Every change is a `pr_state` event on the member's own event stream
  (`amux agent events <member>`), payload
  `{session, pr, changes: [...], wake}`. It is written to the member's amux
  journal (and its structured log for an App Server session); readers that
  predate the type see it as a notice.

## Wake-ups

A meaningful transition — CI turned red, CI green again after red, merge
conflicts appeared, a review was submitted, merged, closed, draft → ready —
queues a short line for the workgroup's coordinator, e.g.

    [amux goal] PR update (amux PR watcher wake-up, not a new task):
    - member 5671a7 PR Kchymet/agent-multiplexer#247: CI failed (check build) — https://…

The first sighting of a PR, CI going pending, and other routine moves are
events only. Lines are de-duplicated by the PR's state hash, coalesced per
coordinator, and delivered through the ordinary steer `prompt` path, but only
when that is safe:

- the coordinator is live — a stopped coordinator is not cold-started (a
  restarted one is told to re-check what it was waiting on anyway);
- it is between turns: not mid-turn, and no permission prompt or question is
  open. Claude's idle reminder also reads "waiting", so a waiting Claude
  coordinator counts as idle only when no permission is open and its
  transcript's last turn ended;
- its goal may be continued: active, waiting, or no goal. A paused, complete,
  blocked, budget-limited or input-waiting goal is the user's to move.

Undeliverable lines wait for up to 2 hours, then are dropped. They are retried
the moment the coordinator's own row changes (its turn ended, its goal moved),
and every 20 seconds as a backstop. For a Claude goal coordinator the prompt starts with the
`[amux goal]` continuation marker, so it continues the current goal rather than
replacing it as a new task would.

## Member wake-ups

Members' activity reaches the coordinator in real time, with no polling on
either side:

- Every activity hook report a member's runtime sends (turn started, tool
  finished, needs the user, turn ended, exited) triggers an immediate rail
  refresh instead of waiting for the 2-second tick.
- Each refresh compares member states with the previous one. These
  transitions queue a line for the coordinator:
  - running or waiting → ready: `member <id> (<title>) finished its turn`;
  - → waiting: `… is waiting on a permission prompt or question`;
  - → idle: `… stopped (its runtime exited)`.
  A member being steered back to running, a first sighting, and the states
  seen at daemon start wake nobody. Claude's idle reminder and login notices
  are not reported as "waiting" at all (`claudecfg.NotificationNeedsUser`), so
  a finished member stays ready.
- Lines share the PR wake-up queue above, including its delivery gating and
  `[amux goal]` marker. A batch that includes them reads
  `[amux goal] Workgroup update (amux wake-up, not a new task): …`.
- Bursts coalesce: the first change opens a 3-second window, everything
  within it goes out as one prompt, and a newer change for the same member
  replaces an older undelivered one (the latest state wins).
- A line is dropped, not delivered late, when the member has moved on by
  delivery time (it is no longer in the state the line reports) or has left
  the workgroup. It is also dropped for a goal coordinator with no goal
  (`none`), which has nothing to continue.
- With PR polling disabled (`AMUX_PR_POLL_INTERVAL=off`), member wake-ups
  still run.

## Goal status

A goal coordinator's rail/status line shows its goal status — `goal active`,
`goal waiting` (it ran `goal wait` and is idle until a wake-up), `goal paused`,
`goal input`, `goal complete`, … or `goal mode (no goal)` — so waiting on a goal
is distinguishable from idle.

## Coordinator status: the workgroup rollup

A coordinator's row reflects the work it dispatched, not only its own turn. Its
state is `enrich.Rollup` (`internal/enrich/rollup.go`), one pure function of
the coordinator's own state, its goal, its members' states and their PRs, used
by the rail, `amux status`, the direct API and the provider alike. First match
wins:

1. **waiting on the user** — the coordinator or a member is `waiting`, or the
   coordinator is between turns with a goal that needs the user (`input`,
   `blocked`, `budgetLimited`; the goal's note is the reason): `state`
   `waiting`.
2. **running** — the coordinator or a member is running (or live and
   unknown). A between-turns coordinator is **supervising**: `state`
   `running`, `subState` `supervising`, e.g.
   `supervising · 2 agents · waiting on m1 running, m2 PR #12 CI running`.
3. **pr_attention** — an open PR needs work (draft, CI red, conflict, changes
   requested, behind). It is the coordinator's job — prwatch wakes it — not the
   user's: `state` `running`, `subState` `pr_attention`; `ready` with reason
   "coordinator stopped" when the coordinator is not running.
4. **waiting_review / waiting_merge** — open PRs waiting on a review, or on a
   merge (approved or no review needed, CI possibly still running), are all
   that is outstanding: `state` `ready`, `subState` the rollup state, status
   e.g. `waiting for merge · 2 PRs · 2 agents · m1 PR #79 approved, …`. Not
   the attention `waiting` badge.
5. `ready` / `idle` otherwise.

The rollup itself is the row's `rollup` object (see
[Session enrichments](session-enrichments.md)); `state` always stays on the
attention ladder.

`amux status --json` adds a `supervision` object to each active coordinator
row: `ownState` (its own turn activity), `goal` and `goalNote`, `members`
(the in-flight members: id, title, state, open-PR summary) and `reason`.
Both fields are additive; a consumer that predates them keeps reading
`state`/`status`.
