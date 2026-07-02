package agent

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
)

// TestHarnessFor maps kinds to harnesses: Claude for ""/"claude", a no-op
// (unknown activity, no restore) for anything else.
func TestHarnessFor(t *testing.T) {
	for _, kind := range []string{"", "claude"} {
		if h := HarnessFor(kind); h.Kind() != "claude" {
			t.Fatalf("HarnessFor(%q).Kind()=%q, want claude", kind, h.Kind())
		}
	}
	h := HarnessFor("hermes")
	if h.Kind() != "hermes" {
		t.Fatalf("HarnessFor(hermes).Kind()=%q", h.Kind())
	}
	if got := h.Activity("anything"); got != engine.ActivityUnknown {
		t.Fatalf("noop Activity=%v, want Unknown", got)
	}
	if restored, err := h.RestoreTranscript("/tmp", "sid"); err != nil || restored {
		t.Fatalf("noop RestoreTranscript restored=%v err=%v", restored, err)
	}

	// Codex is a real harness (not the no-op), with its own kind.
	if c := HarnessFor("codex"); c.Kind() != "codex" {
		t.Fatalf("HarnessFor(codex).Kind()=%q, want codex", c.Kind())
	}
}

// TestCodexActivity exercises the codex harness's rollout-freshness fallback:
// no rollout is Unknown, a just-written rollout is Busy.
func TestCodexActivity(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate HookStateDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	h := HarnessFor("codex")

	const sid = "44444444-4444-4444-8444-444444444444"
	if got := h.Activity(sid); got != engine.ActivityUnknown {
		t.Fatalf("no rollout Activity=%v, want Unknown", got)
	}

	dir := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "07", "02")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	roll := filepath.Join(dir, "rollout-2026-07-02T10-00-00-"+sid+".jsonl")
	if err := os.WriteFile(roll, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := h.Activity(sid); got != engine.ActivityBusy {
		t.Fatalf("fresh rollout Activity=%v, want Busy", got)
	}
}

// TestClaudeActivity maps hook state to engine.Activity: running/waiting are
// Busy, ready/idle are Safe, and a session with no record is Unknown.
func TestClaudeActivity(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // HookStateDir() is under $HOME; isolate
	h := HarnessFor("claude")

	if got := h.Activity("never-seen"); got != engine.ActivityUnknown {
		t.Fatalf("unrecorded session Activity=%v, want Unknown", got)
	}

	cases := map[string]engine.Activity{
		core.StateRunning: engine.ActivityBusy,
		core.StateWaiting: engine.ActivityBusy,
		core.StateReady:   engine.ActivitySafe,
		core.StateIdle:    engine.ActivitySafe,
	}
	for state, want := range cases {
		sid := "sid-" + state
		if err := core.WriteHookState(sid, state, "/cwd"); err != nil {
			t.Fatal(err)
		}
		if got := h.Activity(sid); got != want {
			t.Fatalf("state %q Activity=%v, want %v", state, got, want)
		}
	}
}

// TestClaudeRestoreTranscript verifies the Claude harness restores a captured
// backup into the exact path resume detection reads, so a gap-filled session is
// then resumable.
func TestClaudeRestoreTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
	h := HarnessFor("claude")

	const sid = "33333333-3333-4333-8333-333333333333"
	cwd := t.TempDir()

	// Not resumable and no backup: restore is a no-op.
	if claudecfg.SessionExists(cwd, sid) {
		t.Fatal("session should not exist yet")
	}
	if restored, err := h.RestoreTranscript(cwd, sid); err != nil || restored {
		t.Fatalf("no backup: restored=%v err=%v", restored, err)
	}

	// Capture a backup, then restore: the session becomes resumable.
	live := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(live, []byte(`{"role":"user"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.CaptureTranscript(sid, live, "Stop", ""); err != nil {
		t.Fatal(err)
	}
	restored, err := h.RestoreTranscript(cwd, sid)
	if err != nil || !restored {
		t.Fatalf("restore: restored=%v err=%v", restored, err)
	}
	if !claudecfg.SessionExists(cwd, sid) {
		t.Fatal("session should be resumable after gap-fill")
	}
}

// TestHarnessSkillsAndGuide pins each provider's workspace-config layout: Claude
// reads .claude/skills + CLAUDE.md; any other kind gets the vendor-neutral
// .agents/skills + AGENTS.md.
func TestHarnessSkillsAndGuide(t *testing.T) {
	root := "/ws"
	for _, tc := range []struct {
		kind, skills, guide string
	}{
		{"", ".claude/skills", "CLAUDE.md"},
		{"claude", ".claude/skills", "CLAUDE.md"},
		{"codex", ".agents/skills", "AGENTS.md"},
		{"hermes", ".agents/skills", "AGENTS.md"},
	} {
		h := HarnessFor(tc.kind)
		if got, want := h.SkillsDir(root), filepath.Join(root, tc.skills); got != want {
			t.Errorf("kind %q SkillsDir=%q, want %q", tc.kind, got, want)
		}
		if got, want := h.GuideFile(root), filepath.Join(root, tc.guide); got != want {
			t.Errorf("kind %q GuideFile=%q, want %q", tc.kind, got, want)
		}
	}
}
