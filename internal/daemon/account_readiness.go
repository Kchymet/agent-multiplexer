package daemon

import (
	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
	"context"
	"fmt"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func creationHarnesses(a core.Action) []string {
	member := agent.Canonical(a.Fields["agent"])
	switch a.Action {
	case core.ActionNewWorkgroup, core.ActionCreateWorkspace:
		out := []string{wsops.CoordinatorKind(a.Fields["coordinator"])}
		if a.Action == core.ActionNewWorkgroup && len(store.SplitRepos(a.Fields["repos"])) > 0 || a.Action == core.ActionCreateWorkspace && a.Fields["defaultAgent"] == "1" {
			out = append(out, member)
		}
		return out
	case core.ActionAddAgent:
		return []string{member}
	}
	return nil
}
func checkAccountReadiness(snapshot hp.AccountSnapshot, harnesses []string) error {
	for _, h := range harnesses {
		if h != "claude" && h != "codex" {
			continue
		}
		id := snapshot.Defaults[h]
		found := false
		for _, p := range snapshot.Profiles {
			if p.ID != id {
				continue
			}
			found = true
			if p.State != "ready" || p.Error != "" {
				return fmt.Errorf("%s machine login is not ready (%s); configure Accounts or amux auth login %s", h, p.State, h)
			}
		}
		if !found {
			return fmt.Errorf("%s machine default is unavailable", h)
		}
	}
	return nil
}
func (d *Daemon) accountCreationReady(ctx context.Context, a core.Action) error {
	// A production daemon installs the native owner at startup. Lightweight
	// dispatch-only embedders/tests with no runtime owner do no launch preflight.
	if d.centralCodex == nil {
		return nil
	}
	hs := creationHarnesses(a)
	if len(hs) == 0 {
		return nil
	}
	snapshot, err := d.accountSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("machine account status unavailable")
	}
	return checkAccountReadiness(snapshot, hs)
}
