package claudecfg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSessionLookup verifies the path munging matches Claude Code's project-dir
// naming ('/' and '.' -> '-') and that session detection works.
func TestSessionLookup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	cwd := "/home/u/.local/share/amux/workspaces/abc123"
	const want = "-home-u--local-share-amux-workspaces-abc123" // mirrors real claude munging
	proj := filepath.Join(dir, "projects", want)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "11111111-1111-4111-8111-111111111111"

	if SessionExists(cwd, uuid) {
		t.Fatal("session should not exist before the file is written")
	}
	if AnySession(cwd) {
		t.Fatal("AnySession should be false initially")
	}
	if err := os.WriteFile(filepath.Join(proj, uuid+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SessionExists(cwd, uuid) {
		t.Fatal("session should exist after writing the file (munge mismatch?)")
	}
	if !AnySession(cwd) {
		t.Fatal("AnySession should be true after writing a .jsonl")
	}
}

// TestTurnActive checks that the transcript's last conversational message drives
// the idle/active decision, skipping trailing non-message bookkeeping entries.
func TestTurnActive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	cwd := "/home/u/work"
	proj := filepath.Join(dir, "projects", munge(cwd))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	write := func(uuid string, lines ...string) {
		var s string
		for _, l := range lines {
			s += l + "\n"
		}
		if err := os.WriteFile(filepath.Join(proj, uuid+".jsonl"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const (
		userMsg     = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
		toolResult  = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`
		asstText    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}`
		asstToolUse = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use"}],"stop_reason":"tool_use"}}`
		bookkeeping = `{"type":"queue-operation"}`
	)

	cases := []struct {
		name string
		uuid string
		want bool
		body []string
	}{
		{"finished turn is idle", "a0000000-0000-4000-8000-000000000001", false, []string{userMsg, asstText}},
		{"finished turn under bookkeeping is idle", "a0000000-0000-4000-8000-000000000002", false, []string{userMsg, asstText, bookkeeping}},
		{"pending prompt is active", "a0000000-0000-4000-8000-000000000003", true, []string{asstText, userMsg}},
		{"pending tool call is active", "a0000000-0000-4000-8000-000000000004", true, []string{userMsg, asstToolUse}},
		{"returned tool result is active", "a0000000-0000-4000-8000-000000000005", true, []string{asstToolUse, toolResult}},
	}
	for _, c := range cases {
		write(c.uuid, c.body...)
		if got := TurnActive(cwd, c.uuid); got != c.want {
			t.Errorf("%s: TurnActive=%v want %v", c.name, got, c.want)
		}
	}

	if TurnActive(cwd, "") {
		t.Error("empty uuid should be idle")
	}
	if TurnActive(cwd, "ffffffff-0000-4000-8000-000000000000") {
		t.Error("missing transcript should be idle")
	}
}

// TestPromptBlocked checks that a blocking selection/permission prompt is
// detected while a ready input box or working spinner is not.
func TestPromptBlocked(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{"permission selection", "Bash(rm -rf x)\nDo you want to proceed?\n❯ 1. Yes\n  2. No", true},
		{"edit selection cursor", "Edit file.go\n  1. Yes\n❯ 2. No, tell Claude what to do", true},
		{"ready input box", "│ > Try \"how does X work\"                     │\n  ? for shortcuts", false},
		{"working spinner", "✻ Forming… (12s · esc to interrupt)", false},
		{"plain output", "Here's the summary of what I changed.\nDone.", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := PromptBlocked(c.pane); got != c.want {
			t.Errorf("%s: PromptBlocked=%v want %v", c.name, got, c.want)
		}
	}
}
