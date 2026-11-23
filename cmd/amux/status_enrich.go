package main

import (
	"strings"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// statusEnrichment is the `amux status` table's trailing PR note for a row
// ("" for none): a workgroup's rollup when PRs are what it waits on (or its PR
// data is stale), a member's PR chips.
func statusEnrichment(s core.Session) string {
	var parts []string
	if r := s.Rollup; r != nil {
		switch r.State {
		case hp.RollupPRAttention, hp.RollupWaitingReview, hp.RollupWaitingMerge:
			parts = append(parts, r.Label)
		}
		if r.Stale {
			parts = append(parts, "PR data stale")
		}
	}
	if s.Enrichments != nil {
		var chips []string
		stale := false
		for _, e := range *s.Enrichments {
			if e.PullRequest != nil && (e.PullRequest.State == hp.PRStateOpen || e.PullRequest.State == hp.PRStateDraft) {
				chips = append(chips, e.Label)
				stale = stale || e.Stale
			}
		}
		if len(chips) == 0 && len(*s.Enrichments) > 0 {
			chips = append(chips, (*s.Enrichments)[0].Label)
		}
		if len(chips) > 0 {
			parts = append(parts, "PR "+strings.Join(chips, ", "))
		}
		if stale {
			parts = append(parts, "PR data stale")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  · " + strings.Join(parts, " · ")
}
