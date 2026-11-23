# Session enrichments (session-enrichments-v1)

amux attaches related data to sessions — pull requests first — and derives a
calm workgroup state from it. Types: `harnessproto/enrichment.go`; derivation:
`internal/enrich` (pure functions); data: the PR watcher (`internal/prwatch`,
[PR watch](pr-watch.md)), one `gh` call per repo and branch namespace, never
per client.

## Wire conventions

- Two session keys: `enrichments` (list, on member rows) and `rollup`
  (object, on active coordinator rows). Both are single words, so they read the
  same in the camelCase daemon/harnessproto session, the snake_case direct API
  (`/direct/v1/sessions`) and the cloud backend's projection. The direct API
  also carries `sub_state`.
- Every key nested under them is snake_case on every wire. New timestamps are
  RFC 3339 UTC strings. Both keys are omitted when empty.

## `enrichments`

Order: open items first, then the most recently updated. Envelope (every kind):
`kind`, `id` (stable within the kind), `title`, `url`, `tone`
(`attention|pending|ok|done|neutral`), `label` (a ready-made chip, e.g.
`#245 ✗ CI`), `observed_at`, `stale`, and the kind's payload under a key named
after the kind. Reserved future kinds (not emitted): `issue`, `ci_run`,
`build`, `deploy`, `preview_url`, `firebase_release`.

`pull_request` payload: `repo`, `number`, `title`, `url`, `state`
(`draft|open|merged|closed`), `head_branch`, `base_branch`, `phase`, `attention`,
`ci {status pending|green|red|none, failing_check, failing_checks}`,
`review {state required|changes_requested|approved|none, by, requested}`,
`mergeable {conflict, behind, blocked_by_checks}`, `updated_at`, `merged_at`,
`closed_at`.

`phase` (first match): `attention` (draft, CI red, conflict, changes requested,
behind — reasons in `attention`, in that order), `review` (a review is
required), `merge` (approved or none needed; CI may still be running, which
sets `blocked_by_checks`); merged/closed are `done` and stay listed 3 days.

## `rollup`

`state` (`waiting_user|running|pr_attention|waiting_review|waiting_merge|ready|idle`),
`count`, `label` (e.g. `waiting for merge · 2 PRs`), `reason`,
`enrichments.pull_request {open, draft, attention, waiting_review, waiting_merge, ci_pending, merged, closed}`
(absent with PR watching off), `stale`, `stale_reason`
(`gh_unavailable|rate_limited|error`), `observed_at`. Precedence and the legacy
`state`/`subState` each maps to: [PR watch](pr-watch.md#coordinator-status-the-workgroup-rollup).

## Staleness

An open PR is `stale` when its repo's last listing failed, `gh` is unavailable
(the existing setup condition), or the repo has not been listed for over twice
the poll interval; `observed_at` keeps the last good time. A merged/closed PR
is final, never stale. A rollup is stale when any of its open PRs is, or `gh`
is unavailable; it is still computed from last-known data. The rail and
`amux status` say "PR data stale".

## Compatibility and capability

- `state` stays on the attention ladder; the derived states are in
  `rollup.state` and mirrored into `subState` (an unknown `subState` is
  ignored). An unknown `rollup.state` reads as `ready`; an unknown kind is
  rendered from its envelope or skipped; unknown enum values read as neutral.
- `prs` is still emitted (deprecated).
- Cloud: the provider advertises `session-enrichments-v1` in
  `register.capabilities.features` when its daemon publishes an enrichment kind.
  Direct: `GET /direct/v1/capabilities` carries
  `"enrichments": {"version": 1, "kinds": ["pull_request"]}`. Both come from the
  daemon's host capabilities and are absent with PR watching off or from an
  older daemon. With the flag, no list means "no PRs"; without it, hide the UI.

## Shared helpers

`enrich.Rollup` is the one workgroup rollup (member states, however derived,
feed straight into it). `enrich.MemberSettled` is the shared "member has no
open PR" test for a coordinator retiring a member, `enrich.AllMerged` the
self-archive bar (every PR merged; closed-unmerged means ask), and `enrich.Owns` (=
`ghpr.MatchesBranch`) the shared attribution rule used by the watcher and the
archive guard.

## TUI

Member rows and workgroup headers show tone-colored PR chips; `p` opens the
selected row's PR list in the main pane (Enter/o opens a PR in the browser);
`o` on the rail opens the row's most demanding PR.
