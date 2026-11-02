package main

import (
	"context"
	"errors"
	"fmt"
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
//
// Every harness is optional: a machine advertises exactly the harnesses it
// verified and registers with those. A harness that simply is not installed
// (Hermes on a Claude/Codex machine) is the normal case under automatic
// discovery, not a failure, so it is never reported as one. Discovery reruns
// before every dial; the outcome is logged once and again only when it changes,
// so a reconnect loop for an unrelated reason (a refused dial, a bad token) is
// not interleaved with harness lines that read like its cause.
func executionDiscovery(c providercfg.Config, logf func(string, ...any)) func(context.Context) *harnessproto.ExecutionCapabilities {
	var mu sync.Mutex
	last := ""
	return func(ctx context.Context) *harnessproto.ExecutionCapabilities {
		explicit := providercfg.NormalizeHarnesses(c.Harnesses)
		kinds := explicit
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
		caps, report := summarizeDiscovery(results, len(explicit) > 0)
		mu.Lock()
		if report != last {
			last = report
			logf("%s", report)
		}
		mu.Unlock()
		return caps
	}
}

// summarizeDiscovery folds probe results into the advertisement and a one-line
// operator report. Not-installed harnesses are listed as skipped when discovery
// is automatic and as a problem when the operator allowlisted them; an
// installed harness that fails its version check is always a problem.
func summarizeDiscovery(results []agent.ProbeResult, explicit bool) (*harnessproto.ExecutionCapabilities, string) {
	caps := &harnessproto.ExecutionCapabilities{
		Harnesses:     []harnessproto.HarnessCapability{},
		IdentityModes: []string{harnessproto.IdentityMachine},
	}
	seen := map[string]bool{}
	var advertised, absent, failed []string
	for _, r := range results {
		switch {
		case r.Err == nil:
			if !seen[r.Kind] {
				caps.Harnesses = append(caps.Harnesses, harnessproto.HarnessCapability{Name: r.Kind, Version: r.Version})
				advertised = append(advertised, fmt.Sprintf("%s (%s)", r.Kind, r.Version))
				seen[r.Kind] = true
			}
		case errors.Is(r.Err, agent.ErrNotInstalled) && !explicit:
			absent = append(absent, r.Kind)
		default:
			failed = append(failed, fmt.Sprintf("%s: %v", r.Kind, r.Err))
		}
	}
	report := "harnesses: none verified — no agent can be created on this machine until one is installed"
	if len(advertised) > 0 {
		report = "harnesses: advertising " + strings.Join(advertised, ", ")
	}
	if len(absent) > 0 {
		report += "; not installed (optional, skipped): " + strings.Join(absent, ", ")
	}
	if len(failed) > 0 {
		report += "; unavailable: " + strings.Join(failed, "; ")
	}
	return caps, report
}
