// Package enrich turns what amux observes about a session from outside its
// runtime — pull requests first — into the published session enrichments, and
// derives a workgroup's rollup state from its members and their enrichments.
//
// Everything here is a pure function of its inputs: no I/O, no clock reads
// (callers pass now), no GitHub calls. That keeps one definition of "open",
// "settled" and "what a workgroup is waiting on" for every consumer — the rail,
// `amux status`, the direct API, the coordinator's own done check (MemberSettled)
// and the archive guard (ghpr.MatchesBranch for attribution).
package enrich

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"amux/internal/ghpr"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Retention is how long a merged or closed PR stays listed after it ended.
const Retention = 3 * 24 * time.Hour

// MaxTerminal caps the merged/closed PRs kept per session.
const MaxTerminal = 10

// Owns reports whether a PR head branch belongs to a member branch: the branch
// itself, or the branch plus a "-suffix" or "/suffix". It is ghpr's rule, so
// the PR watcher, the archive guard and the coordinator agree on attribution.
func Owns(branch, head string) bool { return ghpr.MatchesBranch(head, branch) }

// IsOpen reports whether a PR is still open (drafts included).
func IsOpen(pr hp.SessionPR) bool { return pr.State == "open" }

// MemberSettled reports whether a member has no open PR left: every PR it owns
// merged or closed. It is the shared "done" test for a coordinator retiring a
// member and for the UI's rollup.
func MemberSettled(prs []hp.SessionPR) bool {
	for _, pr := range prs {
		if IsOpen(pr) {
			return false
		}
	}
	return true
}

// AllMerged reports whether there is at least one PR and every one merged —
// the coordinator's auto-archive bar (a PR closed without merging, or none at
// all, means it asks the user instead). MemberSettled is the looser "nothing
// left open" test.
func AllMerged(prs []hp.SessionPR) bool {
	for _, pr := range prs {
		if pr.State != "merged" {
			return false
		}
	}
	return len(prs) > 0
}

// EndedAt is when a merged/closed PR ended: GitHub's mergedAt/closedAt, else
// when amux observed the change. Zero for an open PR or when unknown.
func EndedAt(pr hp.SessionPR) time.Time {
	if IsOpen(pr) {
		return time.Time{}
	}
	for _, s := range []string{pr.MergedAt, pr.ClosedAt} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	if pr.Updated > 0 {
		return time.UnixMilli(pr.Updated)
	}
	return time.Time{}
}

// Retained reports whether a PR is still listed at now: every open PR, and a
// merged/closed one until Retention after it ended (kept when the end is
// unknown).
func Retained(pr hp.SessionPR, now time.Time) bool {
	end := EndedAt(pr)
	return end.IsZero() || now.Sub(end) <= Retention
}

// Retain filters prs to those Retained at now, keeping at most MaxTerminal
// merged/closed ones (the most recently ended).
func Retain(prs []hp.SessionPR, now time.Time) []hp.SessionPR {
	var open, done []hp.SessionPR
	for _, pr := range prs {
		switch {
		case IsOpen(pr):
			open = append(open, pr)
		case Retained(pr, now):
			done = append(done, pr)
		}
	}
	if len(done) > MaxTerminal {
		sort.SliceStable(done, func(i, j int) bool { return EndedAt(done[i]).After(EndedAt(done[j])) })
		done = done[:MaxTerminal]
	}
	if len(open)+len(done) == len(prs) {
		return prs
	}
	out := make([]hp.SessionPR, 0, len(open)+len(done))
	keep := map[string]bool{}
	for _, pr := range append(open, done...) {
		keep[Key(pr)] = true
	}
	for _, pr := range prs { // preserve the caller's order
		if keep[Key(pr)] {
			out = append(out, pr)
		}
	}
	return out
}

// Key is a PR's identity: owner/repo#number.
func Key(pr hp.SessionPR) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// CI maps the watcher's check summary to the wire's.
func CI(pr hp.SessionPR) hp.PRCI {
	ci := hp.PRCI{Status: hp.CINone, FailingChecks: nonNil(pr.FailedChecks)}
	switch pr.Checks {
	case "fail":
		ci.Status = hp.CIRed
	case "pending":
		ci.Status = hp.CIPending
	case "pass":
		ci.Status = hp.CIGreen
	}
	if len(pr.FailedChecks) > 0 {
		ci.FailingCheck = pr.FailedChecks[0]
	}
	return ci
}

// Review maps the PR's review decision and latest reviews to the wire's.
func Review(pr hp.SessionPR) hp.PRReviewSummary {
	r := hp.PRReviewSummary{State: hp.ReviewNone, By: []string{}, Requested: nonNil(pr.RequestedReviewers)}
	switch pr.ReviewDecision {
	case "approved":
		r.State = hp.ReviewApproved
	case "changes_requested":
		r.State = hp.ReviewChangesRequested
	case "review_required":
		r.State = hp.ReviewRequired
	}
	for _, rv := range pr.Reviews {
		if rv.Author != "" && (rv.State == "approved" || rv.State == "changes_requested") {
			r.By = append(r.By, rv.Author)
		}
	}
	if len(r.By) == 0 && pr.LastReview != nil && pr.LastReview.Author != "" &&
		(pr.LastReview.State == "approved" || pr.LastReview.State == "changes_requested") {
		r.By = append(r.By, pr.LastReview.Author) // recorded before Reviews existed
	}
	sort.Strings(r.By)
	return r
}

// Mergeable says what stands between the PR and merging.
func Mergeable(pr hp.SessionPR) hp.PRMergeable {
	ci := CI(pr).Status
	return hp.PRMergeable{
		Conflict: pr.Mergeable == "conflicting" || pr.MergeState == "dirty",
		Behind:   pr.MergeState == "behind",
		BlockedByChecks: (ci == hp.CIPending || ci == hp.CIRed) &&
			pr.MergeState != "clean" && pr.MergeState != "has_hooks",
	}
}

// State is the wire state: draft is folded in.
func State(pr hp.SessionPR) string {
	switch pr.State {
	case "open":
		if pr.Draft {
			return hp.PRStateDraft
		}
		return hp.PRStateOpen
	case "merged":
		return hp.PRStateMerged
	case "closed":
		return hp.PRStateClosed
	}
	return pr.State
}

// Phase classifies a PR, first match wins: attention (draft, CI red,
// conflict, changes requested, behind), then review (a review is required),
// then merge (approved or no review needed — CI may still be running); merged
// and closed PRs are done. The attention reasons come in wire order.
func Phase(pr hp.SessionPR) (phase string, attention []string) {
	attention = []string{}
	if !IsOpen(pr) {
		return hp.PRPhaseDone, attention
	}
	m := Mergeable(pr)
	if pr.Draft {
		attention = append(attention, hp.PRAttentionDraft)
	}
	if CI(pr).Status == hp.CIRed {
		attention = append(attention, hp.PRAttentionCIRed)
	}
	if m.Conflict {
		attention = append(attention, hp.PRAttentionConflict)
	}
	rv := Review(pr).State
	if rv == hp.ReviewChangesRequested {
		attention = append(attention, hp.PRAttentionChangesRequested)
	}
	if m.Behind {
		attention = append(attention, hp.PRAttentionBehind)
	}
	switch {
	case len(attention) > 0:
		return hp.PRPhaseAttention, attention
	case rv == hp.ReviewRequired:
		return hp.PRPhaseReview, attention
	default:
		return hp.PRPhaseMerge, attention
	}
}

// Tone is the generic coloring of a PR.
func Tone(pr hp.SessionPR) string {
	phase, _ := Phase(pr)
	switch phase {
	case hp.PRPhaseAttention:
		return hp.ToneAttention
	case hp.PRPhaseReview:
		return hp.TonePending
	case hp.PRPhaseMerge:
		if CI(pr).Status == hp.CIPending {
			return hp.TonePending
		}
		return hp.ToneOK
	}
	if pr.State == "merged" {
		return hp.ToneDone
	}
	return hp.ToneNeutral
}

// Label is a PR's short chip, e.g. "#79 ✓ approved", "#245 ✗ CI".
func Label(pr hp.SessionPR) string {
	return fmt.Sprintf("#%d %s", pr.Number, chip(pr))
}

func chip(pr hp.SessionPR) string {
	phase, attention := Phase(pr)
	switch phase {
	case hp.PRPhaseDone:
		return pr.State
	case hp.PRPhaseAttention:
		has := map[string]bool{}
		for _, a := range attention {
			has[a] = true
		}
		switch {
		case has[hp.PRAttentionCIRed]:
			return "✗ CI"
		case has[hp.PRAttentionConflict]:
			return "✗ conflict"
		case has[hp.PRAttentionChangesRequested]:
			return "✗ changes requested"
		case has[hp.PRAttentionBehind]:
			return "behind"
		}
		return "draft"
	case hp.PRPhaseReview:
		if CI(pr).Status == hp.CIPending {
			return "CI… review"
		}
		return "awaiting review"
	}
	if CI(pr).Status == hp.CIPending {
		return "CI…"
	}
	if Review(pr).State == hp.ReviewApproved {
		return "✓ approved"
	}
	return "✓ ready"
}

// Staleness says how fresh a PR's data is.
type Staleness struct {
	Stale      bool
	ObservedAt time.Time // zero: unknown
}

// PullRequest builds a PR's published enrichment.
func PullRequest(pr hp.SessionPR, st Staleness) hp.Enrichment {
	phase, attention := Phase(pr)
	title := pr.Title
	if title == "" {
		title = Key(pr)
	}
	return hp.Enrichment{
		Kind: hp.EnrichmentPullRequest, ID: Key(pr), Title: title, URL: pr.URL,
		Tone: Tone(pr), Label: Label(pr), ObservedAt: rfc3339(st.ObservedAt),
		Stale: st.Stale && IsOpen(pr), // an ended PR's state is final
		PullRequest: &hp.PullRequestEnrichment{
			Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
			State: State(pr), HeadBranch: pr.Branch, BaseBranch: pr.BaseBranch,
			Phase: phase, Attention: attention,
			CI: CI(pr), Review: Review(pr), Mergeable: Mergeable(pr),
			UpdatedAt: pr.GitHubUpdatedAt, MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt,
		},
	}
}

// Session builds a session's enrichment list from its PRs: retained ones only,
// open first, then the most recently updated. stale reports each PR's
// freshness. Nil when there is nothing to list.
func Session(prs []hp.SessionPR, now time.Time, stale func(hp.SessionPR) Staleness) *hp.Enrichments {
	prs = Retain(prs, now)
	if len(prs) == 0 {
		return nil
	}
	sorted := append([]hp.SessionPR(nil), prs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if IsOpen(a) != IsOpen(b) {
			return IsOpen(a)
		}
		return recency(a).After(recency(b))
	})
	out := make(hp.Enrichments, 0, len(sorted))
	for _, pr := range sorted {
		var st Staleness
		if stale != nil {
			st = stale(pr)
		}
		out = append(out, PullRequest(pr, st))
	}
	return &out
}

// recency orders PRs: GitHub's updatedAt, else when amux saw them change.
func recency(pr hp.SessionPR) time.Time {
	if t, err := time.Parse(time.RFC3339, pr.GitHubUpdatedAt); err == nil {
		return t
	}
	return time.UnixMilli(pr.Updated)
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return append([]string(nil), s...)
}

// oneLine collapses whitespace.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
