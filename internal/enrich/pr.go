// Package enrich turns what amux observes about a session from outside its
// runtime — pull requests first — into the published session enrichments, and
// derives a workgroup's rollup state from its members and their enrichments.
//
// The helpers and the rollup are pure functions of their inputs: no I/O, no
// clock reads (callers pass now), no GitHub calls. The Registry (registry.go)
// runs the Sources (source.go) that produce the items. That keeps one definition of "open",
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

// PRRetention keeps a merged or closed PR listed for 3 days after it ended,
// at most MaxTerminal of them per session.
var PRRetention = Retention{Window: Retention3Days, Max: MaxTerminal}

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

// Status is a PR's normalized status: phase attention → attention, review →
// awaiting_review, merge → awaiting_merge, merged → done, closed → done
// (reason "closed"). Detail is short text such as "CI red: lint".
func Status(pr hp.SessionPR) hp.EnrichmentStatus {
	phase, attention := Phase(pr)
	ci := CI(pr)
	switch phase {
	case hp.PRPhaseAttention:
		return hp.EnrichmentStatus{State: hp.StatusAttention, Detail: attentionDetail(attention, ci), Reasons: attention}
	case hp.PRPhaseReview:
		st := hp.EnrichmentStatus{State: hp.StatusAwaitingReview, Detail: "awaiting review", Reasons: []string{}}
		if ci.Status == hp.CIPending {
			st.Detail, st.Reasons = "awaiting review, CI running", []string{"ci_pending"}
		}
		return st
	case hp.PRPhaseMerge:
		st := hp.EnrichmentStatus{State: hp.StatusAwaitingMerge, Detail: "ready to merge", Reasons: []string{}}
		if Review(pr).State == hp.ReviewApproved {
			st.Detail = "approved"
		}
		if ci.Status == hp.CIPending {
			st.Detail, st.Reasons = st.Detail+", CI running", []string{"ci_pending"}
		}
		return st
	}
	if pr.State == "closed" {
		return hp.EnrichmentStatus{State: hp.StatusDone, Detail: "closed", Reasons: []string{"closed"}}
	}
	if pr.State == "merged" {
		return hp.EnrichmentStatus{State: hp.StatusDone, Detail: "merged", Reasons: []string{"merged"}}
	}
	return hp.EnrichmentStatus{State: hp.StatusUnknown, Detail: pr.State, Reasons: []string{}}
}

func attentionDetail(reasons []string, ci hp.PRCI) string {
	has := map[string]bool{}
	for _, r := range reasons {
		has[r] = true
	}
	switch {
	case has[hp.PRAttentionCIRed]:
		if ci.FailingCheck != "" {
			return "CI red: " + ci.FailingCheck
		}
		return "CI red"
	case has[hp.PRAttentionConflict]:
		return "conflict"
	case has[hp.PRAttentionChangesRequested]:
		return "changes requested"
	case has[hp.PRAttentionBehind]:
		return "behind"
	}
	return "draft"
}

// Tone is the generic coloring of a PR (hp.ToneFor its status).
func Tone(pr hp.SessionPR) string { return hp.ToneFor(Status(pr).State) }

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

// PullRequest builds a PR's published envelope (observed_at and stale are the
// Registry's to fill).
func PullRequest(pr hp.SessionPR) hp.Enrichment {
	phase, attention := Phase(pr)
	title := pr.Title
	if title == "" {
		title = Key(pr)
	}
	st := Status(pr)
	finished := ""
	if !IsOpen(pr) {
		finished = rfc3339(EndedAt(pr))
	}
	return hp.Enrichment{
		Kind: hp.EnrichmentPullRequest, ID: Key(pr), Title: title, URL: pr.URL,
		Status: st, Tone: hp.ToneFor(st.State), Label: Label(pr),
		UpdatedAt: pr.GitHubUpdatedAt, CreatedAt: pr.CreatedAt, FinishedAt: finished,
		PullRequest: &hp.PullRequestEnrichment{
			Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
			State: State(pr), HeadBranch: pr.Branch, BaseBranch: pr.BaseBranch,
			Phase: phase, Attention: attention,
			CI: CI(pr), Review: Review(pr), Mergeable: Mergeable(pr),
			UpdatedAt: pr.GitHubUpdatedAt, MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt,
		},
	}
}

// Recency orders PRs: GitHub's updatedAt, else when amux saw them change.
func Recency(pr hp.SessionPR) time.Time {
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
