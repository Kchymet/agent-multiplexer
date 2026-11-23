package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"amux/internal/enrich"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func prNumbers(prs []harnessproto.SessionPR) string {
	var out []string
	for _, pr := range prs {
		out = append(out, fmt.Sprintf("%s#%d", pr.Repo, pr.Number))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// A member owns every PR whose head is its branch or the branch plus a
// "-suffix" or "/suffix", in every repo it works in, and none of a sibling's.
func TestAttributionMultiplePRsAcrossRepos(t *testing.T) {
	h := newHarness(t)
	h.members = []Member{
		{SessionID: "m1", Workgroup: "wg", Branch: "amux/b1/wg-m1", Repos: []string{"o/a", "o/b"}},
		{SessionID: "m2", Workgroup: "wg", Branch: "amux/b1/wg-m2", Repos: []string{"o/a"}},
	}
	inRepo := func(repo string) func(map[string]any) {
		return func(pr map[string]any) { pr["_repo"] = repo }
	}
	h.gh.set(1, "amux/b1/wg-m1", "OPEN", inRepo("o/a"))
	h.gh.set(2, "amux/b1/wg-m1-docs", "OPEN", inRepo("o/a"))
	h.gh.set(3, "amux/b1/wg-m1/split", "OPEN", inRepo("o/a"))
	h.gh.set(4, "amux/b1/wg-m1", "OPEN", inRepo("o/b"))
	h.gh.set(5, "amux/b1/wg-m2", "OPEN", inRepo("o/a"))
	h.gh.set(6, "amux/b1/wg-m10", "OPEN", inRepo("o/a")) // a longer member id: nobody's
	h.poll()
	if got := prNumbers(h.prs("m1")); got != "o/a#1,o/a#2,o/a#3,o/b#4" {
		t.Fatalf("m1 PRs = %s", got)
	}
	if got := prNumbers(h.prs("m2")); got != "o/a#5" {
		t.Fatalf("m2 PRs = %s", got)
	}
	// One listing per repo and branch namespace, however many members/PRs.
	if n := h.gh.callCount(); n != 2 {
		t.Fatalf("gh calls = %d (%v), want 2", n, h.gh.calls)
	}
}

func TestOwnsMatchesArchiveGuard(t *testing.T) {
	for _, tc := range []struct {
		head string
		want bool
	}{
		{"amux/wg-m1", true}, {"amux/wg-m1-x", true}, {"amux/wg-m1/x", true},
		{"amux/wg-m10", false}, {"amux/wg-m", false}, {"", false},
	} {
		if got := Owns("amux/wg-m1", tc.head); got != tc.want {
			t.Errorf("Owns(%q) = %v, want %v", tc.head, got, tc.want)
		}
	}
}

// The extra fields ride the same gh call.
func TestSummaryCarriesEnrichmentFields(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", func(pr map[string]any) {
		pr["title"] = "add chips"
		pr["baseRefName"] = "master"
		pr["updatedAt"] = "2026-11-20T10:00:00Z"
		pr["mergedAt"] = "0001-01-01T00:00:00Z"
		pr["reviewRequests"] = []any{map[string]any{"login": "carol"}, map[string]any{"slug": "core-team"}}
		pr["latestReviews"] = []any{
			map[string]any{"author": map[string]any{"login": "bob"}, "state": "APPROVED", "submittedAt": "2026-11-20T09:00:00Z"},
			map[string]any{"author": map[string]any{"login": "ann"}, "state": "CHANGES_REQUESTED", "submittedAt": "2026-11-19T09:00:00Z"},
		}
	})
	h.poll()
	prs := h.prs("m1")
	if len(prs) != 1 {
		t.Fatalf("PRs = %+v", prs)
	}
	pr := prs[0]
	if pr.Title != "add chips" || pr.BaseBranch != "master" || pr.GitHubUpdatedAt != "2026-11-20T10:00:00Z" || pr.MergedAt != "" {
		t.Fatalf("summary = %+v", pr)
	}
	if strings.Join(pr.RequestedReviewers, ",") != "carol,core-team" || len(pr.Reviews) != 2 || pr.Reviews[0].Author != "ann" {
		t.Fatalf("reviews = %+v requested = %v", pr.Reviews, pr.RequestedReviewers)
	}
	if !strings.Contains(h.gh.calls[0], "title,baseRefName,updatedAt,mergedAt,closedAt,reviewRequests") {
		t.Fatalf("gh call lacks the enrichment fields: %s", h.gh.calls[0])
	}

	// GitHub's updatedAt alone moving is not a transition.
	h.takeEvents()
	h.gh.update(7, func(pr map[string]any) { pr["updatedAt"] = "2026-11-21T10:00:00Z" })
	h.poll()
	if ev := h.takeEvents(); len(ev) != 0 {
		t.Fatalf("updatedAt bump produced events %+v", ev)
	}
}

// Merged/closed PRs stay listed for three days after they ended.
func TestMergedPRRetainedForThreeDays(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	merged := h.now.Add(time.Minute).UTC().Format(time.RFC3339)
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "MERGED"; pr["mergedAt"] = merged })
	h.poll()
	if got := h.prs("m1"); len(got) != 1 || got[0].State != "merged" || got[0].MergedAt != merged {
		t.Fatalf("PRs after merge = %+v", got)
	}
	h.now = h.now.Add(71 * time.Hour)
	if got := h.prs("m1"); len(got) != 1 {
		t.Fatalf("merged PR dropped before 3 days: %+v", got)
	}
	h.now = h.now.Add(2 * time.Hour)
	if got := h.prs("m1"); len(got) != 0 {
		t.Fatalf("merged PR kept past 3 days: %+v", got)
	}
	h.poll() // the next observation forgets it for good
	if _, ok := h.saved["m1"]; ok {
		t.Fatalf("expired PR still persisted: %s", h.saved["m1"])
	}
}

// Staleness comes from the registry: a failed repo listing marks its PRs
// stale at once (the source knows which), gh missing is the gh_unavailable
// setup condition, and recovery clears both.
func TestPRStaleness(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	stale := func() (bool, enrich.SourceStatus) {
		e := (*h.reg.Enrichments("m1"))[0]
		return e.Stale, h.reg.Sources()[0]
	}
	if s, st := stale(); s || st.Stale || st.Wire().ObservedAt != h.now.UTC().Format(time.RFC3339) || st.Wire().IntervalS != 180 {
		t.Fatalf("fresh: stale=%v status=%+v", s, st)
	}

	h.gh.err = errors.New("HTTP 502: bad gateway")
	h.poll()
	if s, st := stale(); !s || st.Stale || st.LastReason != harnessproto.StaleError {
		t.Fatalf("after a failed listing: stale=%v status=%+v", s, st)
	}

	h.gh.err = fmt.Errorf("exec: %w", exec.ErrNotFound)
	h.poll()
	if s, st := stale(); !s || !st.Stale || st.StaleReason != harnessproto.StaleGHUnavailable {
		t.Fatalf("gh missing: stale=%v status=%+v", s, st)
	}

	h.gh.err = nil
	h.poll()
	if s, st := stale(); s || st.Stale {
		t.Fatalf("after recovery: stale=%v status=%+v", s, st)
	}
}

// A rate limit waits for GitHub's reset (read from the free rate_limit
// endpoint), not just the exponential backoff.
func TestRateLimitRespectsReset(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	reset := h.now.Add(3*time.Minute + 50*time.Minute)
	h.gh.rateLimitReset = reset.Unix()
	h.gh.err = errors.New("GraphQL: API rate limit exceeded for user ID 1")
	h.poll()
	if d := h.delay(); d < 49*time.Minute {
		t.Fatalf("delay %v ignores the reset", d)
	}
}

// deployFake is a second, fake source (a pipeline deploy), registered beside
// the real pull_request one: both land in the same per-session bucket and the
// same rollup. Not shipped.
type deployFake struct{ state string }

func (d *deployFake) Kind() string                { return "deploy" }
func (d *deployFake) Interval() time.Duration     { return time.Minute }
func (d *deployFake) Retention() enrich.Retention { return enrich.Retention{Window: time.Hour, Max: 5} }
func (d *deployFake) Classify(error) enrich.Failure {
	return enrich.Failure{Class: enrich.FailTransient, Reason: harnessproto.StaleError}
}
func (d *deployFake) Poll(_ context.Context, sc enrich.Scope, _ map[string][]enrich.Entry) (enrich.Observation, error) {
	obs := enrich.Observation{Sessions: map[string][]enrich.Entry{}}
	for _, s := range sc.Sessions {
		if s.ID == "m1" {
			obs.Sessions[s.ID] = []enrich.Entry{{Key: "prod", Open: true, Data: []byte(`"` + d.state + `"`)}}
		}
	}
	return obs, nil
}
func (d *deployFake) Render(e enrich.Entry) harnessproto.Enrichment {
	var st string
	_ = json.Unmarshal(e.Data, &st)
	return harnessproto.Enrichment{ID: e.Key, Title: "prod", Label: "prod " + st, Status: harnessproto.EnrichmentStatus{State: st, Detail: st}}
}

func TestPRSourceBesideAnotherKind(t *testing.T) {
	h := newHarness(t)
	dep := &deployFake{state: harnessproto.StatusActive}
	h.reg.Register(dep)
	h.gh.set(7, "amux/wg-m1", "OPEN", func(pr map[string]any) { pr["reviewDecision"] = "APPROVED" })
	h.poll()
	h.reg.PollKind(context.Background(), "deploy")
	list := *h.reg.Enrichments("m1")
	if len(list) != 2 {
		t.Fatalf("enrichments = %+v", list)
	}
	kinds := map[string]string{}
	for _, e := range list {
		kinds[e.Kind] = e.Status.State
	}
	if kinds["pull_request"] != harnessproto.StatusAwaitingMerge || kinds["deploy"] != harnessproto.StatusActive {
		t.Fatalf("kinds = %v", kinds)
	}
	rollup := func() harnessproto.Rollup {
		return enrich.Rollup(enrich.Input{OwnState: harnessproto.StateReady, Sources: h.reg.Sources(),
			Members: []enrich.Member{{ID: "m1", Label: "a", State: harnessproto.StateReady, Enrichments: *h.reg.Enrichments("m1")}}}).Rollup
	}
	if r := rollup(); r.State != harnessproto.RollupWaitingMerge || len(r.Sources) != 2 {
		t.Fatalf("rollup = %+v", r)
	}
	dep.state = harnessproto.StatusFailed
	h.reg.PollKind(context.Background(), "deploy")
	if r := rollup(); r.State != harnessproto.RollupPRAttention || r.Label != "needs attention · 1" {
		t.Fatalf("rollup with a failed deploy = %+v", r)
	}
	// The PR records still decode for the legacy prs field.
	if got := h.prs("m1"); len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("prs = %+v", got)
	}
}

// Notifications are off by default, and when enabled each fires once on entry
// into its phase — never for a PR seen for the first time.
func TestNotifyOptInOncePerEntry(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", func(pr map[string]any) {
		pr["statusCheckRollup"] = []any{check("build", "IN_PROGRESS", "")}
	})
	h.poll()
	h.gh.update(7, func(pr map[string]any) { pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "SUCCESS")} })
	h.poll()
	for _, c := range h.takeEvents() {
		if c.Notify != "" {
			t.Fatalf("notify without opt-in: %+v", c)
		}
	}

	h = newHarness(t)
	h.notify = map[string]bool{NotifyReadyToMerge: true, NotifyNeedsAttention: true}
	h.w = h.build()
	h.gh.set(7, "amux/wg-m1", "OPEN", func(pr map[string]any) {
		pr["statusCheckRollup"] = []any{check("build", "IN_PROGRESS", "")}
	})
	h.poll()
	notified := func() []string {
		var out []string
		for _, c := range h.takeEvents() {
			if c.Notify != "" {
				out = append(out, c.Notify)
			}
		}
		return out
	}
	if got := notified(); len(got) != 0 {
		t.Fatalf("first sight notified %v", got)
	}
	h.gh.update(7, func(pr map[string]any) { pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "SUCCESS")} })
	h.poll()
	if got := notified(); strings.Join(got, ",") != NotifyReadyToMerge {
		t.Fatalf("CI green notified %v", got)
	}
	h.gh.update(7, func(pr map[string]any) { pr["title"] = "renamed" })
	h.poll()
	if got := notified(); len(got) != 0 {
		t.Fatalf("still-ready PR re-notified %v", got)
	}
	h.gh.update(7, func(pr map[string]any) { pr["mergeable"] = "CONFLICTING"; pr["mergeStateStatus"] = "DIRTY" })
	h.poll()
	if got := notified(); strings.Join(got, ",") != NotifyNeedsAttention {
		t.Fatalf("conflict notified %v", got)
	}
}
