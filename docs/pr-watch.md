# PR watch: member PR state and coordinator wake-ups

amux knows which GitHub pull requests each workgroup member owns and wakes the
member's coordinator when one of them changes state, so a coordinator that runs
`amux agent goal wait` (or simply ends its turn) while members' PRs are in
flight has a real wake-up source (`internal/prwatch`, wired in
`internal/daemon/prwatch.go`).

## Ownership

A member owns the PRs whose head branch is its branch (`amux/<wg>-<member>`) or
that branch plus a `-suffix` (`amux/<wg>-<member>-b`), in the GitHub
repositories it works in (a tracked repo whose source is a GitHub URL or
`owner/name`; other repos are skipped). Only active members of active
workgroups are watched.

## Polling

The daemon polls on the host with the host user's own `gh` login (agents hold no
GitHub authority for it). Every 3 minutes (`AMUX_PR_POLL_INTERVAL`, a Go
duration such as `90s`, minimum 30s; `off` disables) it runs one
`gh pr list --state open --search head:amux/ --json …` per repository — whatever
the number of members — and a single `gh pr view` only for a tracked PR that
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
  `pass|fail|pending`, failedChecks, reviewDecision, lastReview, updated). The
  rail/status text gains a short suffix, e.g. `· PR #247 CI✗ conflict`.
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

Undeliverable lines wait (retried every 20 seconds) for up to 2 hours, then are
dropped. For a Claude goal coordinator the prompt starts with the
`[amux goal]` continuation marker, so it continues the current goal rather than
replacing it as a new task would.

## Goal status

A goal coordinator's rail/status line shows its goal status — `goal active`,
`goal waiting` (it ran `goal wait` and is idle until a wake-up), `goal paused`,
`goal input`, `goal complete`, … or `goal mode (no goal)` — so waiting on a goal
is distinguishable from idle.
