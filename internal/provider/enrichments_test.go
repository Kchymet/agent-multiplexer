package provider

import (
	"context"
	"net"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The provider advertises session-enrichments-v1 only alongside published
// sessions and only when its daemon publishes an enrichment kind.
func TestEnrichmentsFeature(t *testing.T) {
	for _, tc := range []struct {
		name    string
		publish bool
		kinds   []string
		want    bool
	}{
		{"daemon publishes PRs", true, []string{harnessproto.EnrichmentPullRequest}, true},
		{"PR watching off", true, nil, false},
		{"sessions not published", false, []string{harnessproto.EnrichmentPullRequest}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conns := make(chan net.Conn, 1)
			src := &mutableSource{}
			cfg := Config{
				Orchestrator: "pipe", Dial: pipeDialer(conns),
				EnrichmentKinds: func(context.Context) ([]string, error) { return tc.kinds, nil },
			}
			if tc.publish {
				cfg.PublishSessions, cfg.Sessions = true, src.poll
			}
			p := newFast(cfg)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go p.Run(ctx)
			reg := expectRegister(t, harnessproto.NewConn(<-conns))
			if got := hasFeature(reg, harnessproto.SessionEnrichmentsFeature); got != tc.want {
				t.Fatalf("features = %v, want enrichments=%v", reg.Capabilities.Features, tc.want)
			}
		})
	}
}
