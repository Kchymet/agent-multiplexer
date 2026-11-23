package harnessbin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordingSource reports the kind's state as seen while its install runs.
type recordingSource struct {
	*fakeSource
	m      *Manager
	during State
}

func (r *recordingSource) Fetch(ctx context.Context, version, dir string) error {
	r.during = r.m.LoadState(r.kind)
	return r.fakeSource.Fetch(ctx, version, dir)
}

// Every adoption decision is recorded with its reason, declines included, and
// an allowed one is marked in progress until it finishes, so `amux harness ls`
// and `amux doctor` can say why a harness was or was not adopted.
func TestAdoptRecordsEveryDecision(t *testing.T) {
	m, claude := newTestManager(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return now }
	rec := &recordingSource{fakeSource: claude, m: m}
	m.Sources["claude"] = rec
	inputs := map[string]AdoptInput{
		"claude": {OnPath: "/home/u/.local/bin/claude"},
		"codex":  {},
	}
	var seen []AdoptResult
	res := m.Adopt(context.Background(), Settings{}, func(k string) AdoptInput { return inputs[k] }, func(r AdoptResult) { seen = append(seen, r) })
	if len(seen) != 2 || !seen[0].Decided || seen[0].Adopted || seen[1].Decided {
		t.Fatalf("decided callbacks = %+v", seen)
	}
	if !res[0].Adopted || res[0].RecordErr != nil || res[1].RecordErr != nil {
		t.Fatalf("Adopt = %+v", res)
	}
	if rec.during.AdoptStartedAt.IsZero() || rec.during.AdoptDecision != AdoptDecisionAdopt {
		t.Fatalf("state during install = %+v; want an in-progress adopt decision", rec.during)
	}
	st := m.LoadState("claude")
	if st.AdoptDecision != AdoptDecisionAdopt || !strings.Contains(st.AdoptReason, "found /home/u/.local/bin/claude") ||
		!st.AdoptCheckedAt.Equal(now) || !st.AdoptStartedAt.IsZero() || st.AdoptError != "" {
		t.Fatalf("claude state after adoption = %+v", st)
	}
	st = m.LoadState("codex")
	if st.AdoptDecision != AdoptDecisionSkip || !strings.Contains(st.AdoptReason, "not installed or logged in") || !st.AdoptCheckedAt.Equal(now) {
		t.Fatalf("codex decline not recorded: %+v", st)
	}

	// A failed attempt ends the in-progress mark and keeps its error.
	codex := m.Sources["codex"].(*fakeSource)
	codex.fail = errors.New("offline")
	inputs["codex"] = AdoptInput{LoggedIn: "auth.json"}
	if res := m.Adopt(context.Background(), Settings{}, func(k string) AdoptInput { return inputs[k] }); res[1].Err == nil {
		t.Fatalf("Adopt = %+v", res)
	}
	if st := m.LoadState("codex"); !st.AdoptStartedAt.IsZero() || st.AdoptError != "offline" || st.AdoptDecision != AdoptDecisionAdopt {
		t.Fatalf("codex state after failure = %+v", st)
	}
}

// A downloaded binary whose --version leaves a helper holding stdout open
// must not hang the install (and with it the daemon's adoption loop).
func TestVerifyDoesNotWaitForInheritedStdout(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 2.1.0\nsleep 30 &\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Root: t.TempDir()}
	start := time.Now()
	out, err := m.verify(context.Background(), bin)
	if err != nil || out != "2.1.0" {
		t.Fatalf("verify = %q, %v", out, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("verify waited %v for an orphaned helper's stdout", d)
	}
}
