package daemon

import (
	"context"
	"log"
	"strings"
	"time"

	"amux/internal/harnessbin"
)

// harnessUpdatePoll is how often the daemon re-reads the harnesses settings
// and asks whether a managed harness's update check is due. The check itself
// runs at most once per configured interval (default 24h), measured from the
// last recorded check, so daemon restarts do not re-query release servers.
const harnessUpdatePoll = 15 * time.Minute

// harnessUpdateLoop is the daemon-scheduled update check for amux-managed
// harness installs (harnesses.update: off | notify | auto). It never touches a
// harness that has no managed install, and never a running session: an
// activation only moves the current link that future launches resolve.
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
		t.Reset(harnessUpdatePoll)
	}
}

func runHarnessUpdateCheck(ctx context.Context, m *harnessbin.Manager, now time.Time) []harnessbin.CheckResult {
	settings, err := harnessbin.LoadSettings()
	if err != nil {
		log.Printf("harness updates: %v", err)
		return nil
	}
	if settings.UpdateMode() == harnessbin.UpdateOff || !harnessCheckDue(m, settings.Interval(), now) {
		return nil
	}
	results := m.Check(ctx, settings)
	for _, r := range results {
		switch {
		case r.Err != nil:
			log.Printf("harness updates: %s: %v", r.Kind, r.Err)
		case r.Updated:
			log.Printf("harness updates: %s updated to %s (running sessions keep their version; removed %s)", r.Kind, r.Current, strings.Join(r.Removed, ", "))
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
