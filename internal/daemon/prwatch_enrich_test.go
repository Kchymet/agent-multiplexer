package daemon

import (
	"testing"

	"amux/internal/prwatch"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestPRNotifyKinds(t *testing.T) {
	for in, want := range map[string]int{"": 0, "ready_to_merge": 1, "ready_to_merge, needs_attention": 2, "all": 2, "bogus": 0} {
		t.Setenv(prNotifyEnv, in)
		if got := prNotifyKinds(); len(got) != want {
			t.Errorf("%s=%q: %v, want %d kinds", prNotifyEnv, in, got, want)
		}
	}
	t.Setenv(prNotifyEnv, "")
	if prNotifyKinds()[prwatch.NotifyNeedsAttention] {
		t.Fatal("needs_attention on by default")
	}
}

// The host capability lists pull_request (and its freshness) only while PR
// watching is on: the source is registered only then.
func TestEnrichmentKinds(t *testing.T) {
	d := &Daemon{}
	if got := d.enrichmentKinds(); got != nil {
		t.Fatalf("no registry: %v", got)
	}
	t.Setenv(prPollIntervalEnv, "off")
	d.enrich = d.newEnrichments()
	if got := d.enrichmentKinds(); got != nil || d.enrichmentSources() != nil {
		t.Fatalf("polling off: %v", got)
	}
	t.Setenv(prPollIntervalEnv, "")
	d.enrich = d.newEnrichments()
	if got := d.enrichmentKinds(); len(got) != 1 || got[0] != harnessproto.EnrichmentPullRequest {
		t.Fatalf("polling on: %v", got)
	}
	if src := d.enrichmentSources(); len(src) != 1 || src[0].Kind != harnessproto.EnrichmentPullRequest || src[0].IntervalS != 180 {
		t.Fatalf("sources = %+v", src)
	}
}
