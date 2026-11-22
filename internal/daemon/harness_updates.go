package daemon

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/harnessbin"
)

// harnessUpdatePoll is how often the daemon re-reads the harnesses settings
// and asks whether a managed harness's update check is due. The check itself
// runs at most once per configured interval (default 24h), measured from the
// last recorded check, so daemon restarts do not re-query release servers.
const harnessUpdatePoll = 15 * time.Minute

// harnessUpdateLoop is the daemon-scheduled lifecycle for amux-managed harness
// installs (harnesses.update: auto by default, notify, or off; the
// AMUX_NO_HARNESS_AUTO_UPDATE environment variable forces off). In auto mode it
// first adopts a managed install for each harness the user already uses (see
// harnessbin.AdoptDecision), then installs and activates new releases. It
// never touches a running session itself: an activation only moves the
// current link, and harnessRestartLoop moves running sessions onto the new
// version at a safe point.
func (d *Daemon) harnessUpdateLoop(ctx context.Context) {
	m := harnessbin.New()
	t := time.NewTimer(2 * time.Minute) // let startup settle before network I/O
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		runHarnessUpdateCheck(ctx, m, time.Now())
		d.kickHarnessRestart()
		t.Reset(harnessUpdatePoll)
	}
}

// harnessAdoptInput is a seam for tests.
var harnessAdoptInput = agent.AdoptInput

func runHarnessUpdateCheck(ctx context.Context, m *harnessbin.Manager, now time.Time) []harnessbin.CheckResult {
	settings, err := harnessbin.LoadSettings()
	if err != nil {
		log.Printf("harness updates: %v", err)
		return nil
	}
	mode := settings.EffectiveUpdateMode(os.Getenv)
	if mode == harnessbin.UpdateOff {
		return nil
	}
	if mode == harnessbin.UpdateAuto {
		for _, r := range m.Adopt(ctx, settings, func(kind string) harnessbin.AdoptInput { return harnessAdoptInput(ctx, kind) }) {
			switch {
			case r.Err != nil:
				log.Printf("harness updates: adopting %s (%s): %v", r.Kind, r.Reason, r.Err)
			case r.Adopted:
				log.Printf("harness updates: adopted an amux-managed %s %s (%s); new sessions launch it (opt out: amux config set harnesses.update off)", r.Kind, r.Version, r.Reason)
			}
		}
	}
	if !harnessCheckDue(m, settings.Interval(), now) {
		return nil
	}
	results := m.Check(ctx, settings)
	for _, r := range results {
		switch {
		case r.Err != nil:
			log.Printf("harness updates: %s: %v", r.Kind, r.Err)
		case r.Updated:
			log.Printf("harness updates: %s updated to %s (running sessions move onto it when idle unless harnesses.restart_sessions is off; removed %s)", r.Kind, r.Current, strings.Join(r.Removed, ", "))
		case harnessbin.CompareVersions(r.Available, r.Current) > 0:
			log.Printf("harness updates: %s %s available (current %s); run amux harness update %s", r.Kind, r.Available, r.Current, r.Kind)
		}
	}
	return results
}

// harnessCheckDue reports whether any managed harness's last check is older
// than interval.
func harnessCheckDue(m *harnessbin.Manager, interval time.Duration, now time.Time) bool {
	for _, kind := range harnessbin.Kinds {
		if m.Current(kind) == "" {
			continue
		}
		if st := m.LoadState(kind); st.CheckedAt.IsZero() || now.Sub(st.CheckedAt) >= interval {
			return true
		}
	}
	return false
}
