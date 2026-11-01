package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
)

// TestNativeClaudeSubmitsPromptSentRightAfterLaunch is the web dashboard's
// creation flow against the real Claude Code TUI: creation starts the agent with
// no client attached, and the browser's `prompt` arrives milliseconds later. The
// payload is the daemon's own (steerPayload + holdUntilBooted), so this fails if
// that prompt is typed into a Claude that is still booting — which drops the
// submit key and leaves the conversation empty.
//
// No credentials, network or model turn: with a synthetic account record and no
// login, Claude Code reaches its composer ("Not logged in") and still records a
// submitted prompt as a user turn in its transcript. The transcript, not the
// screen, is what tells submitted text from text left sitting in the composer.
// CI sets AMUX_TEST_CLAUDE_BIN to its pinned fixture.
func TestNativeClaudeSubmitsPromptSentRightAfterLaunch(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CLAUDE_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CLAUDE_BIN to a Claude Code binary")
	}
	cfg, dir := t.TempDir(), t.TempDir()
	state, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		// An account record skips the first-run login picker.
		"oauthAccount": map[string]any{
			"accountUuid": "00000000-0000-4000-8000-000000000001", "emailAddress": "fixture@example.invalid",
			"organizationUuid": "00000000-0000-4000-8000-000000000002",
		},
		"projects": map[string]any{dir: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}},
	})
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const conversation = "11111111-2222-4333-8444-555555555555"
	const marker = "WEB-PROMPT-MARKER"

	eng := local.New()
	t.Cleanup(eng.Shutdown)
	// Inherited nesting markers (this test often runs inside a Claude session)
	// would stop Claude journalling; blank them, then set the fixture's home.
	env := append(blankEnv("CLAUDE"), "CLAUDE_CONFIG_DIR="+cfg, "TERM=xterm-256color")
	in, err := eng.Ensure(context.Background(), engine.Spec{
		Key: engine.Key{AgentID: "boot"}, Dir: dir, Env: env,
		Argv: []string{bin, "--session-id", conversation},
	})
	if err != nil {
		t.Fatalf("spawn %s: %v", bin, err)
	}
	var mu sync.Mutex
	var out []byte
	t.Cleanup(in.Subscribe(engine.Sink{Output: func(b []byte) {
		mu.Lock()
		out = append(out, b...)
		mu.Unlock()
	}}))

	payload, err := steerPayload(core.SteerPrompt, agent.HarnessFor("claude").Keys(), map[string]string{core.SteerText: marker + " creation status"})
	if err != nil {
		t.Fatal(err)
	}
	(&Daemon{}).holdUntilBooted(payload)
	in.InputSequence(payload) // right after launch, as the browser's prompt arrives

	transcript := filepath.Join(cfg, "projects", strings.NewReplacer("/", "-", ".", "-").Replace(dir), conversation+".jsonl")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(transcript); err == nil && strings.Contains(string(b), marker) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	mu.Lock()
	tail := string(out[max(0, len(out)-2000):])
	mu.Unlock()
	t.Fatalf("a prompt sent right after launch never became a user turn in %s; terminal tail:\n%q", transcript, tail)
}

// blankEnv overrides inherited variables with the prefix (Spec.Env only adds; an
// empty value reads as unset). Callers append their own values after it.
func blankEnv(prefix string) []string {
	var out []string
	for _, e := range os.Environ() {
		if k, _, ok := strings.Cut(e, "="); ok && strings.HasPrefix(k, prefix) {
			out = append(out, k+"=")
		}
	}
	return out
}
