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

// The host capability lists pull_request only while PR watching is on.
func TestEnrichmentKinds(t *testing.T) {
	d := &Daemon{}
	if got := d.enrichmentKinds(); got != nil {
		t.Fatalf("no watcher: %v", got)
	}
	d.prs = prwatch.New(prwatch.Config{NoPoll: true})
	if got := d.enrichmentKinds(); got != nil {
		t.Fatalf("polling off: %v", got)
	}
	d.prs = prwatch.New(prwatch.Config{})
	if got := d.enrichmentKinds(); len(got) != 1 || got[0] != harnessproto.EnrichmentPullRequest {
		t.Fatalf("polling on: %v", got)
	}
}
