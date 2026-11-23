package main

import (
	"testing"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestStatusEnrichment(t *testing.T) {
	item := func(kind, label, state string, stale bool) hp.Enrichment {
		return hp.Enrichment{Kind: kind, Label: label, Stale: stale, Status: hp.EnrichmentStatus{State: state}}
	}
	pr := hp.EnrichmentPullRequest
	open := hp.Enrichments{item(pr, "#79 ✓ approved", hp.StatusAwaitingMerge, false), item(pr, "#80 ✗ CI", hp.StatusAttention, true),
		item(pr, "#9 merged", hp.StatusDone, false), item("deploy", "prod failed", hp.StatusFailed, false)}
	done := hp.Enrichments{item(pr, "#9 merged", hp.StatusDone, false)}
	for _, tc := range []struct {
		name string
		s    core.Session
		want string
	}{
		{"plain row", core.Session{ID: "a"}, ""},
		{"calm workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupWaitingMerge, Label: "waiting for merge · 2 PRs"}}, "  · waiting for merge · 2 PRs"},
		{"running workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupRunning, Label: "running"}}, ""},
		{"stale workgroup", core.Session{Rollup: &hp.Rollup{State: hp.RollupReady, Stale: true}}, "  · PR data stale"},
		{"member with open items", core.Session{Enrichments: &open}, "  · PR #79 ✓ approved, PR #80 ✗ CI, deploy prod failed · PR data stale"},
		{"member with a merged PR", core.Session{Enrichments: &done}, "  · PR #9 merged"},
	} {
		if got := statusEnrichment(tc.s); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
