package daemon

import (
	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
	"context"
	"fmt"
	"sync"
	"time"

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
	// Creation runs under the daemon-wide effect gate, and a fresh snapshot does
	// native account IO (seconds per Codex profile) that would stall every
	// session's reports. A recent snapshot that already shows the harnesses
	// ready is enough to admit; anything else is re-probed.
	if cached, ok := d.readiness.recent(d.accountTime()); ok && checkAccountReadiness(cached, hs) == nil {
		return nil
	}
	snapshot, err := d.accountSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("machine account status unavailable")
	}
	return checkAccountReadiness(snapshot, hs)
}

// accountReadinessReuse is how long a ready snapshot admits creations.
const accountReadinessReuse = time.Minute

type accountReadinessCache struct {
	mu       sync.Mutex
	snapshot hp.AccountSnapshot
	at       time.Time
}

func (c *accountReadinessCache) store(snapshot hp.AccountSnapshot, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot, c.at = snapshot, at
}

// clear forgets the snapshot after any account change, so a logout or mode
// switch is never admitted from stale readiness.
func (c *accountReadinessCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot, c.at = hp.AccountSnapshot{}, time.Time{}
}

func (c *accountReadinessCache) recent(now time.Time) (hp.AccountSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || now.Before(c.at) || now.Sub(c.at) >= accountReadinessReuse {
		return hp.AccountSnapshot{}, false
	}
	return c.snapshot, true
}
