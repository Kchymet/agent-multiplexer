package harnessbin

import (
	"context"
	"errors"
	"time"
)

// CheckResult is one harness's outcome of a scheduled update check.
type CheckResult struct {
	Kind, Current, Available string
	Updated                  bool
	Removed                  []string
	Err                      error
}

// Check runs one update pass over every harness that has a managed install
// (amux never installs a harness the user has not opted into). In notify mode
// it records the newest available version; in auto mode it also installs and
// activates it unless the harness is pinned, then garbage-collects. Running
// sessions are unaffected: they keep executing their resolved version dir.
func (m *Manager) Check(ctx context.Context, settings Settings) []CheckResult {
	mode := settings.UpdateMode()
	if mode == UpdateOff {
		return nil
	}
	var out []CheckResult
	for _, kind := range Kinds {
		cur := m.Current(kind)
		if cur == "" {
			continue
		}
		r := CheckResult{Kind: kind, Current: cur}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		r.Available, r.Err = m.Resolve(cctx, kind, settings.Kind(kind).Channel)
		cancel()
		_ = m.RecordCheck(kind, r.Available, r.Err)
		if r.Err == nil && mode == UpdateAuto && CompareVersions(r.Available, cur) > 0 {
			ictx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			_, to, err := m.Update(ictx, kind, r.Available)
			cancel()
			switch {
			case errors.Is(err, ErrPinned):
				// Pinned: notify-only for this harness.
			case err != nil:
				r.Err = err
			default:
				r.Updated, r.Current = true, to
				r.Removed, _, _ = m.GC(kind)
			}
		}
		out = append(out, r)
	}
	return out
}
