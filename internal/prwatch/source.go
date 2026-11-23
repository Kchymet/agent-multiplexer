package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"amux/internal/enrich"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Source is the pull_request enrichment source: it lists the open PRs of each
// GitHub repository that tracked sessions work in — one `gh pr list` per repo
// and branch prefix, however many members and PRs — attributes each PR to the
// member whose branch heads it (enrich.Owns), reads a tracked PR that left the
// open list once with `gh pr view`, and reports every transition. The
// enrich.Registry schedules it, backs it off, persists its records and applies
// retention; coordinator wake-ups ride its changes (Change.Wake) to a Watcher.
type Source struct {
	cfg SourceConfig
}

// SourceConfig wires a Source. Run is required.
type SourceConfig struct {
	Run      Runner
	Interval time.Duration // default DefaultInterval
	// Notify enables user notification kinds (Notify* values) on Change;
	// empty: none (the default).
	Notify map[string]bool
	Now    func() time.Time
}

// NewSource builds the pull_request source.
func NewSource(cfg SourceConfig) *Source {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Source{cfg: cfg}
}

// Kind is pull_request.
func (s *Source) Kind() string { return harnessproto.EnrichmentPullRequest }

// Interval is the poll interval.
func (s *Source) Interval() time.Duration { return s.cfg.Interval }

// Retention keeps merged/closed PRs 3 days, at most 10 per session.
func (s *Source) Retention() enrich.Retention { return enrich.PRRetention }

// Render is a PR record's published envelope.
func (s *Source) Render(it enrich.Entry) harnessproto.Enrichment {
	var rec record
	_ = json.Unmarshal(it.Data, &rec)
	return enrich.PullRequest(rec.PR)
}

// rateLimitError is a rate-limit failure with when the limit resets.
type rateLimitError struct {
	err     error
	resetAt time.Time
}

func (e *rateLimitError) Error() string { return e.err.Error() }
func (e *rateLimitError) Unwrap() error { return e.err }

// Classify maps gh failures onto the registry's classes: a missing or
// unauthenticated gh is the gh_unavailable setup condition; a rate limit
// carries its reset time when gh could tell.
func (s *Source) Classify(err error) enrich.Failure {
	switch classify(err) {
	case errSetup:
		return enrich.Failure{Class: enrich.FailSetup, Reason: harnessproto.StaleGHUnavailable}
	case errRateLimit:
		f := enrich.Failure{Class: enrich.FailRateLimit, Reason: harnessproto.StaleRateLimited}
		var rl *rateLimitError
		if errors.As(err, &rl) {
			f.ResetAt = rl.resetAt
		}
		return f
	}
	return enrich.Failure{Class: enrich.FailTransient, Reason: harnessproto.StaleError}
}

// rateLimitReset asks GitHub when the GraphQL rate limit (which `gh pr list`
// spends) resets; zero when it cannot tell. The rate_limit endpoint itself is
// free.
func rateLimitReset(ctx context.Context, run Runner) time.Time {
	out, err := run(ctx, "api", "rate_limit")
	if err != nil {
		return time.Time{}
	}
	var rl struct {
		Resources map[string]struct {
			Reset int64 `json:"reset"`
		} `json:"resources"`
	}
	if json.Unmarshal(out, &rl) != nil {
		return time.Time{}
	}
	for _, k := range []string{"graphql", "core"} {
		if r := rl.Resources[k].Reset; r > 0 {
			return time.Unix(r, 0)
		}
	}
	return time.Time{}
}

// Poll lists the scope's PRs and folds them into each member's records.
func (s *Source) Poll(ctx context.Context, scope enrich.Scope, prev map[string][]enrich.Entry) (enrich.Observation, error) {
	// One query per (repo, branch namespace): this broker's members share the
	// "amux/<broker>/" prefix (pre-broker members share "amux/"), so this is
	// normally one gh call per repo, and it never lists another broker's
	// branches. Ownership is still decided per member by Owns below.
	type query struct{ repo, prefix, head string }
	queries := map[query]bool{}
	for _, m := range scope.Sessions {
		if m.Branch == "" {
			continue
		}
		for _, repo := range m.Repos {
			if i := strings.LastIndex(m.Branch, "/"); i > 0 {
				queries[query{repo: repo, prefix: m.Branch[:i+1]}] = true
			} else {
				queries[query{repo: repo, head: m.Branch}] = true
			}
		}
	}
	keys := make([]query, 0, len(queries))
	for q := range queries {
		keys = append(keys, q)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].prefix+keys[i].head < keys[j].prefix+keys[j].head
	})
	open := map[string][]harnessproto.SessionPR{} // by repo
	failed := map[string]bool{}                   // repos whose listing failed this tick
	var firstErr error
	for _, q := range keys {
		if failed[q.repo] {
			continue
		}
		var prs []harnessproto.SessionPR
		var err error
		if q.head != "" {
			prs, err = listHead(ctx, s.cfg.Run, q.repo, q.head)
		} else {
			prs, err = listOpen(ctx, s.cfg.Run, q.repo, q.prefix)
		}
		if err != nil {
			if ctx.Err() != nil {
				return enrich.Observation{}, ctx.Err()
			}
			failed[q.repo] = true
			if firstErr == nil {
				firstErr = err
			}
			if classify(err) == errSetup {
				// gh itself is unusable: every other call would fail the same
				// way, and nothing observed this tick is trustworthy.
				return enrich.Observation{}, err
			}
			continue
		}
		open[q.repo] = append(open[q.repo], prs...)
	}
	if firstErr != nil && classify(firstErr) == errRateLimit {
		firstErr = &rateLimitError{err: firstErr, resetAt: rateLimitReset(ctx, s.cfg.Run)}
	}

	now := s.cfg.Now()
	obs := enrich.Observation{Sessions: map[string][]enrich.Entry{}}
	for _, m := range scope.Sessions {
		items, changes := s.observe(ctx, m, prev[m.ID], open, failed, now)
		obs.Sessions[m.ID] = items
		obs.Changes = append(obs.Changes, changes...)
	}
	return obs, firstErr
}

// observe folds this tick's observation into one member's records.
func (s *Source) observe(ctx context.Context, m enrich.ScopeSession, prevItems []enrich.Entry, open map[string][]harnessproto.SessionPR, failed map[string]bool, now time.Time) ([]enrich.Entry, []enrich.Change) {
	type prevRec struct {
		rec  record
		item enrich.Entry
	}
	var prev []prevRec
	prevBy := map[string]prevRec{}
	for _, it := range prevItems {
		var rec record
		if json.Unmarshal(it.Data, &rec) != nil {
			continue
		}
		p := prevRec{rec: rec, item: it}
		prev = append(prev, p)
		prevBy[prKey(rec.PR)] = p
	}
	inRepo := map[string]bool{}
	for _, r := range m.Repos {
		inRepo[r] = true
	}

	var next []enrich.Entry
	var changes []enrich.Change
	seen := map[string]bool{}
	add := func(prior *record, cur harnessproto.SessionPR) {
		rec, ch, wake := transition(prior, cur, now)
		next = append(next, recordEntry(rec, now, false))
		seen[prKey(cur)] = true
		if len(ch) > 0 {
			c := Change{SessionID: m.ID, Workgroup: m.Workgroup, Label: m.Label, Branch: m.Branch, PR: rec.PR, Changes: ch, Wake: wake}
			if prior != nil {
				if kind := notifyKind(prior.PR, rec.PR); s.cfg.Notify[kind] {
					c.Notify = kind
				}
			}
			changes = append(changes, enrich.Change{SessionID: m.ID, Key: prKey(rec.PR), Changes: ch, Payload: c})
		}
	}
	for _, repo := range m.Repos {
		for _, pr := range open[repo] {
			if !Owns(m.Branch, pr.Branch) || seen[prKey(pr)] {
				continue
			}
			if p, ok := prevBy[prKey(pr)]; ok {
				add(&p.rec, pr)
			} else {
				add(nil, pr)
			}
		}
	}
	for _, p := range prev {
		k := prKey(p.rec.PR)
		if seen[k] {
			continue
		}
		seen[k] = true
		// A tracked PR in a repo the member no longer works in, or whose listing
		// failed this tick, or one already merged/closed: keep it as it was
		// (last-known — stale when its repo failed).
		if p.rec.PR.State != "open" || !inRepo[p.rec.PR.Repo] || failed[p.rec.PR.Repo] {
			it := p.item
			it.Stale = failed[p.rec.PR.Repo]
			next = append(next, it)
			continue
		}
		// It left the open list: learn how.
		cur, err := view(ctx, s.cfg.Run, p.rec.PR.Repo, p.rec.PR.Number)
		if err != nil || cur.State == "" {
			it := p.item
			it.Stale = true
			next = append(next, it)
			continue
		}
		rec := p.rec
		add(&rec, cur)
	}
	// Newest first per repo, as the watcher always listed them.
	sort.SliceStable(next, func(i, j int) bool {
		ri, ni := splitKey(next[i].Key)
		rj, nj := splitKey(next[j].Key)
		if ri != rj {
			return ri < rj
		}
		return ni > nj
	})
	return next, changes
}

// splitKey splits owner/repo#N.
func splitKey(k string) (string, int) {
	i := strings.LastIndex(k, "#")
	if i < 0 {
		return k, 0
	}
	n, _ := strconv.Atoi(k[i+1:])
	return k[:i], n
}

// recordEntry wraps a PR record as a registry item.
func recordEntry(rec record, observed time.Time, stale bool) enrich.Entry {
	data, _ := json.Marshal(rec)
	return enrich.Entry{
		Key: prKey(rec.PR), Open: enrich.IsOpen(rec.PR), EndedAt: enrich.EndedAt(rec.PR),
		SortAt: enrich.Recency(rec.PR), ObservedAt: observed, Stale: stale, Data: data,
	}
}

// PRs decodes a session's pull_request items back into PR summaries (the
// legacy `prs` field and the rail's PR suffix), in item order.
func PRs(items []enrich.Entry) []harnessproto.SessionPR {
	var out []harnessproto.SessionPR
	for _, it := range items {
		var rec record
		if json.Unmarshal(it.Data, &rec) == nil && rec.PR.Number != 0 {
			out = append(out, rec.PR)
		}
	}
	return out
}

// DecodeLegacy reads a PR watcher document from before the enrichment
// registry (a JSON array of records) as pull_request items.
func DecodeLegacy(doc []byte) (string, []enrich.Entry, bool) {
	var recs []record
	if json.Unmarshal(doc, &recs) != nil {
		return "", nil, false
	}
	items := make([]enrich.Entry, 0, len(recs))
	for _, rec := range recs {
		var observed time.Time
		if rec.PR.Updated > 0 {
			observed = time.UnixMilli(rec.PR.Updated)
		}
		items = append(items, recordEntry(rec, observed, false))
	}
	return harnessproto.EnrichmentPullRequest, items, true
}
