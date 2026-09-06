package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"amux/internal/agent"
	"amux/internal/providercfg"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func executionConfig(f provideFlags, file providercfg.Config, getenv func(string) string) (providercfg.Config, error) {
	c := file
	c.IdentityMode = firstNonEmpty(f.identityMode, getenv("AMUX_PROVIDER_IDENTITY_MODE"), file.IdentityMode, harnessproto.IdentityMachine)
	if v := getenv("AMUX_PROVIDER_HARNESSES"); v != "" {
		c.Harnesses = strings.Split(v, ",")
	}
	if len(f.harnesses) > 0 {
		c.Harnesses = append([]string(nil), f.harnesses...)
	}
	c.Harnesses = providercfg.NormalizeHarnesses(c.Harnesses)
	return c, c.ValidateExecution()
}

// Discovery executes in the host provider environment. In particular it must not
// use the registering agent's private CODEX_HOME/CLAUDE_CONFIG_DIR as evidence
// of host authentication. Only executable readiness is advertised here.
func executionDiscovery(c providercfg.Config) func(context.Context) *harnessproto.ExecutionCapabilities {
	return func(ctx context.Context) *harnessproto.ExecutionCapabilities {
		kinds := providercfg.NormalizeHarnesses(c.Harnesses)
		if len(kinds) == 0 {
			kinds = agent.Kinds()
		}
		results := make([]agent.ProbeResult, len(kinds))
		var wg sync.WaitGroup
		for i, kind := range kinds {
			wg.Add(1)
			go func(i int, kind string) { defer wg.Done(); results[i] = agent.Probe(ctx, kind) }(i, kind)
		}
		wg.Wait()
		caps := &harnessproto.ExecutionCapabilities{
			Harnesses:     []harnessproto.HarnessCapability{},
			IdentityModes: []string{harnessproto.IdentityMachine},
		}
		seen := map[string]bool{}
		for _, r := range results {
			if r.Err != nil {
				fmt.Fprintf(os.Stderr, "amux provide: harness %s unavailable: %v\n", r.Kind, r.Err)
				continue
			}
			if !seen[r.Kind] {
				caps.Harnesses = append(caps.Harnesses, harnessproto.HarnessCapability{Name: r.Kind, Version: r.Version})
				seen[r.Kind] = true
			}
		}
		return caps
	}
}
