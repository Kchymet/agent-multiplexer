package main

import (
	"testing"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestStatusEnrichment(t *testing.T) {
	pr := func(label, state string, stale bool) hp.Enrichment {
		return hp.Enrichment{Kind: hp.EnrichmentPullRequest, Label: label, Stale: stale, PullRequest: &hp.PullRequestEnrichment{State: state}}
	}
	open := hp.Enrichments{pr("#79 ✓ approved", hp.PRStateOpen, false), pr("#80 ✗ CI", hp.PRStateDraft, true), pr("#9 merged", hp.PRStateMerged, false)}
	done := hp.Enrichments{pr("#9 merged", hp.PRStateMerged, false)}
	for _, tc := range []struct {
		name string
		s    core.Session
		want string
	}{
		{"plain row", core.Session{ID: "a"}, ""},
		{"calm workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupWaitingMerge, Label: "waiting for merge · 2 PRs"}}, "  · waiting for merge · 2 PRs"},
		{"running workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupRunning, Label: "running"}}, ""},
		{"stale workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupReady, Stale: true}}, "  · PR data stale"},
		{"member with open PRs", core.Session{Enrichments: &open}, "  · PR #79 ✓ approved, #80 ✗ CI · PR data stale"},
		{"member with a merged PR", core.Session{Enrichments: &done}, "  · PR #9 merged"},
	} {
		if got := statusEnrichment(tc.s); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
