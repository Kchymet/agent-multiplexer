package main

import (
	"strings"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// statusEnrichment is the `amux status` table's trailing note for a row (""
// for none): a workgroup's rollup when enrichments are what it waits on (or
// their data is stale), a member's open enrichments of every kind.
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
			if e.Status.State != hp.StatusDone {
				chips = append(chips, enrichmentChip(e))
				stale = stale || e.Stale
			}
		}
		if len(chips) == 0 && len(*s.Enrichments) > 0 {
			chips = append(chips, enrichmentChip((*s.Enrichments)[0]))
		}
		if len(chips) > 0 {
			parts = append(parts, strings.Join(chips, ", "))
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

// enrichmentChip is one item in plain text: "PR #79 ✓ approved",
// "deploy prod failed".
func enrichmentChip(e hp.Enrichment) string {
	noun := e.Kind
	if e.Kind == hp.EnrichmentPullRequest {
		noun = "PR"
	}
	label := e.Label
	if label == "" {
		label = e.Title
	}
	return noun + " " + label
}
