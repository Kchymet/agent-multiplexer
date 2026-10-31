package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/claudegoal"
)

// A PR wake-up typed into a Claude goal coordinator must continue its goal,
// never replace the objective as a new task would.
func TestPRWakeContinuesClaudeGoal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	goal := claudegoal.State{Objective: "ship PR B", Status: claudegoal.StatusActive, Note: "waiting on CI", Created: 1}
	text := prWakeText(true, []string{"member m1 PR o/r#7: CI failed (check build) — https://github.com/o/r/pull/7"})
	if !strings.HasPrefix(text, prWakeMarker) {
		t.Fatalf("wake %q lacks the goal marker", text)
	}
	next := claudegoal.OnPrompt(goal, text, now)
	if next.Objective != goal.Objective || next.Status != claudegoal.StatusActive {
		t.Fatalf("wake replaced the goal: %+v", next)
	}
	// With no goal, the wake-up establishes none.
	if got := claudegoal.OnPrompt(claudegoal.State{}, text, now); !got.None() {
		t.Fatalf("wake established a goal: %+v", got)
	}
	if plain := prWakeText(false, []string{"x"}); strings.HasPrefix(plain, prWakeMarker) {
		t.Fatalf("non-goal wake %q carries the goal marker", plain)
	}
}

func TestLastTurnEnded(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ended := write(
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}`,
		`{"type":"system","subtype":"stop_hook_summary"}`,
	)
	if !lastTurnEnded(ended) {
		t.Fatal("finished turn not recognized as idle")
	}
	pending := write(
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{}}],"stop_reason":"tool_use"}}`,
	)
	if lastTurnEnded(pending) {
		t.Fatal("pending tool call recognized as idle")
	}
	if lastTurnEnded(filepath.Join(dir, "missing")) {
		t.Fatal("missing transcript recognized as idle")
	}
}
