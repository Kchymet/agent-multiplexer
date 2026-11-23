package harnessproto

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
// issue (Linear/GitHub), ci_run, build, deploy, preview_url, firebase_release.
const (
	EnrichmentPullRequest = "pull_request"
)

// Enrichment tones (Enrichment.Tone): generic coloring for any kind.
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
	Kind  string `json:"kind"`
	ID    string `json:"id"` // stable within its kind, e.g. owner/repo#79
	Title string `json:"title"`
	URL   string `json:"url"`
	Tone  string `json:"tone"`  // a Tone* value
	Label string `json:"label"` // a short ready-made chip, e.g. "#245 ✗ CI"
	// ObservedAt is the last successful observation (RFC 3339).
	ObservedAt string `json:"observed_at"`
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
}

// RollupEnrichments counts a workgroup's enrichments by kind; a kind is absent
// when the daemon does not observe it (PR watching off).
type RollupEnrichments struct {
	PullRequest *PullRequestCounts `json:"pull_request,omitempty"`
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
