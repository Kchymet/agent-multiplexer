# Session enrichments (session-enrichments v1.1)

amux attaches things related to a session — pull requests first — as one
generic bucket of **enrichments**, and derives a calm workgroup state from
them. Wire types: `harnessproto/enrichment.go`. Daemon side: `internal/enrich`.

## Sources and the registry

Each kind comes from a **source** (`enrich.Source`: `Kind`, `Interval`,
`Retention`, `Poll(ctx, scope, prev)`, `Classify(err)`, `Render(entry)`). The
daemon's `enrich.Registry` runs every registered source on its own interval and
does the rest generically:

- **One poll per source per scope.** The scope is the tracked sessions with
  their branches and repos; nothing is fetched per client.
- **Backoff.** Exponential from the interval, capped at 30 minutes, and never
  before a rate limit resets (the source reports the reset; for PRs, GitHub's
  free `rate_limit` endpoint).
- **Setup conditions** (e.g. `gh_unavailable`: gh missing or logged out) are
  logged once, retried every 30 minutes, and stale at once.
- **Staleness.** A source is stale with a setup condition or with no successful
  poll within twice its interval. An open entry is stale when its source is, or
  when its part of the scope failed (e.g. its repo's listing).
- **Persistence and retention.** The last observation per session per kind
  persists (the store's per-session document, `{"v":2,"kinds":{…}}`; the PR
  watcher's older format is read as `pull_request`). Finished entries are kept
  per kind (PRs: 3 days, at most 10 per session).
- **Transitions.** Each is emitted: PRs as `pr_state` (with coordinator
  wake-ups, unchanged), any other kind as `enrichment_state`
  (`{session, kind, id, changes, enrichment}`).

`internal/prwatch` is the `pull_request` source: one `gh pr list` per repo and
branch prefix, attribution via `ghpr.MatchesBranch` (`enrich.Owns`).

Future kinds (not built): `issue` (Linear/GitHub issues), `ci_run` (CI runs),
`build` / `deploy` (pipeline-engine builds and deploys), `preview_url`
(preview deployments), `firebase_release` (Firebase App Distribution).

## Wire conventions

- Session keys: `enrichments` (list, member rows) and `rollup` (object, active
  coordinator rows). The direct API also carries `sub_state`.
- Every nested key is snake_case on every wire; timestamps are RFC 3339 UTC.
- Everything is additive: consumers ignore unknown keys.

## `enrichments`

Order: open entries first, then the most recently updated. Every entry, any
kind:

| key | |
|---|---|
| `kind` | e.g. `pull_request`; unknown kinds render generically (icon + status chip) |
| `id`, `url` | identity (`url` may be empty) |
| `title` | display title |
| `status` | `{state, detail, reasons}` — the normalized status (below) |
| `tone` | `attention\|pending\|ok\|done\|neutral`, derived from `status.state` (v1 compat) |
| `label` | ready-made chip text, e.g. `#245 ✗ CI` |
| `observed_at` | last successful observation |
| `updated_at` | when the thing itself last changed at its source |
| `created_at`, `finished_at` | optional; `finished_at` for done/failed (merged, closed, finished) |
| `stale` | data is last-known (see Staleness) |
| `<kind>` | the kind's payload, under a key named after the kind (`pull_request`) |

**Normalized states** (`status.state`), the same for every kind:

| state | meaning | tone | drives |
|---|---|---|---|
| `attention` | needs work from the agents (CI red, conflict) | attention | `pr_attention` |
| `failed` | finished unsuccessfully (a failed deploy) | attention | `pr_attention` |
| `awaiting_review` | waiting on a human review | pending | `waiting_review` |
| `awaiting_merge` | waiting on a human to merge/ship | ok | `waiting_merge` |
| `pending` | machines are working (CI running, build queued) | pending | — |
| `active` | live and current (a running deploy, an up preview) | pending | — |
| `ok` | settled and fine (a passing CI run) | ok | — |
| `done` | finished (merged, closed, released) | done | — |
| `unknown` | the source cannot say; also any value a consumer does not know | neutral | — |

`status.detail` is short kind-specific text (`CI red: lint`); `status.reasons`
are machine-readable, kind-specific reasons (PR: `draft`, `ci_red`,
`conflict`, `changes_requested`, `behind`, `ci_pending`, `merged`, `closed`).

**PR mapping.** `phase` attention → `attention`, review → `awaiting_review`,
merge → `awaiting_merge` (CI may still be running: reason `ci_pending`),
merged → `done` (reason `merged`), closed → `done` (reason `closed`).

`pull_request` payload (unchanged from v1): `repo`, `number`, `title`, `url`,
`state` (`draft|open|merged|closed`), `head_branch`, `base_branch`, `phase`,
`attention`, `ci {status, failing_check, failing_checks}`,
`review {state, by, requested}`, `mergeable {conflict, behind, blocked_by_checks}`,
`updated_at`, `merged_at`, `closed_at`.

## `rollup`

`state` (`waiting_user|running|pr_attention|waiting_review|waiting_merge|ready|idle`;
unknown reads as `ready`), `count`, `label`, `reason`, `enrichments`
(`pull_request` keeps its v1 counts; any other kind is counted by normalized
state under its own key), `totals` (all kinds, by normalized state, every state
present), `sources` (`[{kind, stale, stale_reason, observed_at, interval_s}]`),
`stale`, `stale_reason` (`gh_unavailable|rate_limited|error`, or a source's own
setup reason), `observed_at`.

The rollup (`enrich.Rollup`) is a pure function of member states and entries'
normalized statuses — never a kind's payload. Precedence: waiting on the user >
running > `pr_attention` > `waiting_review` / `waiting_merge` > ready/idle
([PR watch](pr-watch.md#coordinator-status-the-workgroup-rollup)). Labels name
PRs when only PRs contribute (`PR attention · 2 PRs`, `waiting for merge · 1 PR`)
and are generic otherwise (`needs attention · 2`, `waiting for merge · 3`).

## Compatibility and capability

- `state` stays on the attention ladder; the derived states are in
  `rollup.state` and mirrored into `subState`.
- `prs` is still emitted (deprecated).
- Cloud: the provider advertises `session-enrichments-v1` when its daemon
  publishes a kind. Direct: `GET /direct/v1/capabilities` carries
  `"enrichments": {"version": 1, "kinds": [...], "sources": [...]}`. Both are
  absent with no source registered (PR watching off) or from an older daemon.

## Shared helpers

`enrich.Rollup` is the one workgroup rollup. `enrich.MemberSettled` is the
shared "member has no open PR" test, `enrich.AllMerged` the self-archive bar
(every PR merged; closed-unmerged means ask), and `enrich.Owns` the shared
attribution rule.

## TUI

Member rows and workgroup headers show chips — the kind's icon (`⑂` PR, `⇪`
deploy, `•` unknown) and the label, colored by `status.state`; `p` opens the
selected row's list in the main pane (Enter/o opens an entry in the browser);
`o` on the rail opens the row's most demanding one.
