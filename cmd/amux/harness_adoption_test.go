package main

import (
	"strings"
	"testing"
	"time"

	"amux/internal/harnessbin"
)

// harness ls and doctor say why a harness has no managed install: the
// daemon's last decision (declines included), an attempt that never finished,
// or a failure.
func TestAdoptionStatusExplainsTheLastDecision(t *testing.T) {
	at := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	cases := []struct {
		st   harnessbin.State
		want string
	}{
		{harnessbin.State{}, "no decision recorded yet"},
		{harnessbin.State{AdoptCheckedAt: at, AdoptDecision: harnessbin.AdoptDecisionSkip, AdoptReason: "claude is not installed or logged in"}, "skipped at " + at.Local().Format(time.RFC3339) + ": claude is not installed or logged in"},
		{harnessbin.State{AdoptCheckedAt: at, AdoptStartedAt: at, AdoptDecision: harnessbin.AdoptDecisionAdopt, AdoptReason: "found /u/.local/bin/claude"}, "installing since"},
		{harnessbin.State{AdoptCheckedAt: at, AdoptAttemptAt: at, AdoptDecision: harnessbin.AdoptDecisionAdopt, AdoptError: "GET x: 503"}, "failed at " + at.Local().Format(time.RFC3339) + ": GET x: 503"},
		{harnessbin.State{AdoptAttemptAt: at, AdoptError: "offline"}, "failed at"}, // recorded by an older amux
		{harnessbin.State{AdoptCheckedAt: at, AdoptAttemptAt: at, AdoptDecision: harnessbin.AdoptDecisionAdopt, AdoptReason: "found x"}, "adopted at"},
	}
	for _, c := range cases {
		if got := adoptionStatus(c.st); !strings.Contains(got, c.want) {
			t.Errorf("adoptionStatus(%+v) = %q; want it to contain %q", c.st, got, c.want)
		}
	}
}
