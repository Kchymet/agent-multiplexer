package harnessproto

import "encoding/json"

// Session enrichments: data amux attaches to a session from outside the
// runtime (pull requests first), and the workgroup rollup derived from them.
//
// Wire contract (session-enrichments-v1): the two session keys, `enrichments`
// and `rollup`, are single words so they read the same in this camelCase
// harnessproto Session, the snake_case direct API and the cloud backend's
// projection; every key nested under them is snake_case on every wire, so one
// decoder serves all three. New timestamps are RFC 3339 UTC strings.
//
// Compatibility rules a consumer follows:
//   - Session.State never carries a rollup state: it stays in the closed
//     attention ladder. The derived states live in Rollup.State and are
//     mirrored into Session.SubState; an unknown SubState is ignored.
//   - An unknown Rollup.State reads as RollupReady.
//   - An unknown Enrichment.Kind is rendered generically from Title, URL,
//     Tone and Label, or skipped; an unknown enum value inside a payload reads
//     as neutral.
//   - The feature SessionEnrichmentsFeature (cloud) or the direct
//     capabilities' `enrichments` block says the daemon emits them: with it, an
//     absent list means "none"; without it, a consumer hides the UI.

// SessionEnrichmentsFeature is the register.capabilities.features string a
// provider advertises when its daemon emits session enrichments (with PR
// watching on).
const SessionEnrichmentsFeature = "session-enrichments-v1"

// EnrichmentsVersion is the enrichment wire version; it changes only for a
// breaking change.
const EnrichmentsVersion = 1

// Enrichment kinds (Enrichment.Kind). Reserved for later, not emitted yet:
// issue (Linear/GitHub issues), ci_run (CI runs), build and deploy
// (pipeline-engine builds/deploys), preview_url (preview deployments),
// firebase_release (Firebase App Distribution releases).
const (
	EnrichmentPullRequest = "pull_request"
)

// Normalized item states (EnrichmentStatus.State), the same for every kind. The
// rollup reads only these, never a kind's payload, so a new kind contributes
// to a workgroup's state by mapping its own states onto them:
//
//	attention        needs work from the agents (CI red, conflict, a failed check)
//	awaiting_review  waiting on a human review
//	awaiting_merge   waiting on a human to merge/ship
//	pending          machines are working on it (CI running, a build queued)
//	active           live and current (a running deploy, a preview URL that is up)
//	ok               settled and fine, nothing to do (a passing CI run)
//	done             finished (merged, closed, released)
//	failed           finished unsuccessfully (a failed deploy)
//	unknown          the source cannot say
//
// Consumers read an unknown value as unknown (neutral).
const (
	StatusAttention      = "attention"
	StatusAwaitingReview = "awaiting_review"
	StatusAwaitingMerge  = "awaiting_merge"
	StatusPending        = "pending"
	StatusActive         = "active"
	StatusOK             = "ok"
	StatusDone           = "done"
	StatusFailed         = "failed"
	StatusUnknown        = "unknown"
)

// EnrichmentStates lists the normalized states in rollup order (Rollup.Totals).
var EnrichmentStates = []string{StatusAttention, StatusFailed, StatusAwaitingReview, StatusAwaitingMerge,
	StatusPending, StatusActive, StatusOK, StatusDone, StatusUnknown}

// EnrichmentStatus is an item's normalized status (Enrichment.Status).
type EnrichmentStatus struct {
	State   string   `json:"state"`   // a Status* value
	Detail  string   `json:"detail"`  // short kind-specific text, e.g. "CI red: lint"
	Reasons []string `json:"reasons"` // machine-readable reasons, kind-specific (e.g. ci_red, closed)
}

// ToneFor is the tone a normalized state implies (Enrichment.Tone).
func ToneFor(state string) string {
	switch state {
	case StatusAttention, StatusFailed:
		return ToneAttention
	case StatusAwaitingReview, StatusPending, StatusActive:
		return TonePending
	case StatusAwaitingMerge, StatusOK:
		return ToneOK
	case StatusDone:
		return ToneDone
	}
	return ToneNeutral
}

// Enrichment tones (Enrichment.Tone): generic coloring for any kind, derived
// from Status.State (ToneFor). Kept for consumers that predate Status.
const (
	ToneAttention = "attention"
	TonePending   = "pending"
	ToneOK        = "ok"
	ToneDone      = "done"
	ToneNeutral   = "neutral"
)

// Pull request states (PullRequestEnrichment.State).
const (
	PRStateDraft  = "draft"
	PRStateOpen   = "open"
	PRStateMerged = "merged"
	PRStateClosed = "closed"
)

// Pull request phases (PullRequestEnrichment.Phase), derived by amux.
const (
	PRPhaseAttention = "attention"
	PRPhaseReview    = "review"
	PRPhaseMerge     = "merge"
	PRPhaseDone      = "done"
)

// Attention reasons (PullRequestEnrichment.Attention), in this order.
const (
	PRAttentionDraft            = "draft"
	PRAttentionCIRed            = "ci_red"
	PRAttentionConflict         = "conflict"
	PRAttentionChangesRequested = "changes_requested"
	PRAttentionBehind           = "behind"
)

// CI statuses (PRCI.Status).
const (
	CIPending = "pending"
	CIGreen   = "green"
	CIRed     = "red"
	CINone    = "none"
)

// Review states (PRReviewSummary.State).
const (
	ReviewRequired         = "required"
	ReviewChangesRequested = "changes_requested"
	ReviewApproved         = "approved"
	ReviewNone             = "none"
)

// Enrichments is Session.Enrichments' list: open items first, then newest.
type Enrichments []Enrichment

// Enrichment is one piece of related data attached to a session. The envelope
// is the same for every kind; the kind's payload sits under a key named after
// the kind.
type Enrichment struct {
	Kind   string           `json:"kind"`
	ID     string           `json:"id"` // stable within its kind, e.g. owner/repo#79
	Title  string           `json:"title"`
	URL    string           `json:"url"`
	Status EnrichmentStatus `json:"status"` // normalized status (spec v1.1)
	Tone   string           `json:"tone"`   // a Tone* value, ToneFor(Status.State)
	Label  string           `json:"label"`  // a short ready-made chip, e.g. "#245 ✗ CI"
	// ObservedAt is the last successful observation (RFC 3339).
	ObservedAt string `json:"observed_at"`
	// UpdatedAt is when the item itself last changed at its source;
	// CreatedAt and FinishedAt (done/failed items: merged, closed, finished)
	// are set when known. RFC 3339; spec v1.1.
	UpdatedAt  string `json:"updated_at"`
	CreatedAt  string `json:"created_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	// Stale: the item's data source failed on its last poll (or has not
	// answered for over twice its poll interval); the data is last-known.
	Stale       bool                   `json:"stale"`
	PullRequest *PullRequestEnrichment `json:"pull_request,omitempty"`
}

// PullRequestEnrichment is the pull_request payload.
type PullRequestEnrichment struct {
	Repo       string          `json:"repo"`
	Number     int             `json:"number"`
	Title      string          `json:"title"`
	URL        string          `json:"url"`
	State      string          `json:"state"` // a PRState* value
	HeadBranch string          `json:"head_branch"`
	BaseBranch string          `json:"base_branch"`
	Phase      string          `json:"phase"`     // a PRPhase* value
	Attention  []string        `json:"attention"` // PRAttention* reasons (phase attention)
	CI         PRCI            `json:"ci"`
	Review     PRReviewSummary `json:"review"`
	Mergeable  PRMergeable     `json:"mergeable"`
	UpdatedAt  string          `json:"updated_at"` // GitHub's updatedAt
	MergedAt   string          `json:"merged_at"`
	ClosedAt   string          `json:"closed_at"`
}

// PRCI is a PR's status-check rollup.
type PRCI struct {
	Status        string   `json:"status"`        // a CI* value
	FailingCheck  string   `json:"failing_check"` // the first failing check by name
	FailingChecks []string `json:"failing_checks"`
}

// PRReviewSummary is a PR's review situation.
type PRReviewSummary struct {
	State     string   `json:"state"`     // a Review* value
	By        []string `json:"by"`        // latest reviewers who approved or requested changes
	Requested []string `json:"requested"` // pending requested reviewers
}

// PRMergeable is what stands between a PR and merging.
type PRMergeable struct {
	Conflict        bool `json:"conflict"`
	Behind          bool `json:"behind"`
	BlockedByChecks bool `json:"blocked_by_checks"`
}

// Rollup states (Rollup.State), in precedence order.
const (
	RollupWaitingUser   = "waiting_user"
	RollupRunning       = "running"
	RollupPRAttention   = "pr_attention"
	RollupWaitingReview = "waiting_review"
	RollupWaitingMerge  = "waiting_merge"
	RollupReady         = "ready"
	RollupIdle          = "idle"
)

// Rollup stale reasons (Rollup.StaleReason).
const (
	StaleGHUnavailable = "gh_unavailable"
	StaleRateLimited   = "rate_limited"
	StaleError         = "error"
)

// Rollup is a workgroup coordinator row's derived state (Session.Rollup).
type Rollup struct {
	State string `json:"state"` // a Rollup* value; unknown reads as ready
	// Count is what drives State: members waiting or running, or PRs in the
	// state's tier.
	Count       int               `json:"count"`
	Label       string            `json:"label"` // e.g. "waiting for merge · 2 PRs"
	Reason      string            `json:"reason"`
	Enrichments RollupEnrichments `json:"enrichments"`
	Stale       bool              `json:"stale"`
	StaleReason string            `json:"stale_reason"` // a Stale* value, or ""
	ObservedAt  string            `json:"observed_at"`
	// Totals counts the workgroup's retained items of every kind by
	// normalized state (every EnrichmentStates key present). Spec v1.1.
	Totals map[string]int `json:"totals"`
	// Sources is each enrichment source's freshness. Spec v1.1.
	Sources []EnrichmentSource `json:"sources"`
	// PullRequests lists the members' open pull requests (drafts included,
	// merged and closed left out), the most demanding first, so a consumer can
	// link each one straight from the workgroup row. Absent when there are none
	// or PR watching is off; older daemons never send it. Spec v1.2.
	PullRequests []RollupPullRequest `json:"pull_requests,omitempty"`
}

// RollupPullRequest is one member's open pull request (Rollup.PullRequests):
// enough to draw a chip and open the PR without the member row.
type RollupPullRequest struct {
	SessionID string `json:"session_id"` // the member session that owns it
	Member    string `json:"member"`     // the member's compact label
	ID        string `json:"id"`         // the enrichment id, owner/repo#n
	Repo      string `json:"repo"`       // owner/repo
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`    // the PR on GitHub (may be empty)
	State     string `json:"state"`  // a PRState* value: draft or open
	Status    string `json:"status"` // its normalized Status.State
	CI        string `json:"ci"`     // a CI* value
	Label     string `json:"label"`  // the ready-made chip, e.g. "#245 ✗ CI"
}

// EnrichmentSource is one enrichment source's freshness (Rollup.Sources and
// the direct capabilities' enrichments.sources).
type EnrichmentSource struct {
	Kind        string `json:"kind"`
	Stale       bool   `json:"stale"`        // no successful poll within twice its interval, or a setup condition
	StaleReason string `json:"stale_reason"` // a Stale* value (a source may add its own setup reason), or ""
	ObservedAt  string `json:"observed_at"`  // its last successful poll (RFC 3339), or ""
	IntervalS   int    `json:"interval_s"`   // its poll interval in seconds
}

// RollupEnrichments counts a workgroup's enrichments by kind; a kind is absent
// when the daemon does not observe it (PR watching off). pull_request keeps its
// v1 counts; every other kind is counted by normalized state (Other, encoded
// beside it under the kind's own key).
type RollupEnrichments struct {
	PullRequest *PullRequestCounts `json:"pull_request,omitempty"`
	// Other is per kind (not pull_request): count by normalized state.
	Other map[string]map[string]int `json:"-"`
}

// MarshalJSON encodes pull_request and every Other kind as sibling keys.
func (r RollupEnrichments) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	for k, v := range r.Other {
		if k != EnrichmentPullRequest {
			m[k] = v
		}
	}
	if r.PullRequest != nil {
		m[EnrichmentPullRequest] = r.PullRequest
	}
	return json.Marshal(m)
}

// UnmarshalJSON is MarshalJSON's inverse.
func (r *RollupEnrichments) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*r = RollupEnrichments{}
	for k, v := range raw {
		if k == EnrichmentPullRequest {
			var c PullRequestCounts
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			r.PullRequest = &c
			continue
		}
		var c map[string]int
		if json.Unmarshal(v, &c) == nil {
			if r.Other == nil {
				r.Other = map[string]map[string]int{}
			}
			r.Other[k] = c
		}
	}
	return nil
}

// PullRequestCounts counts a workgroup's PRs. Open counts every open PR,
// drafts included; Merged and Closed count those still in the retention window.
type PullRequestCounts struct {
	Open          int `json:"open"`
	Draft         int `json:"draft"`
	Attention     int `json:"attention"`
	WaitingReview int `json:"waiting_review"`
	WaitingMerge  int `json:"waiting_merge"`
	CIPending     int `json:"ci_pending"`
	Merged        int `json:"merged"`
	Closed        int `json:"closed"`
}
