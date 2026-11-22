package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/hostprep"
	"amux/internal/store"
)

func withAdoptSeams(t *testing.T, path func(string) string, version string, keychain bool) {
	t.Helper()
	oldPath, oldVersion, oldKeychain, oldConfig := adoptLookPath, adoptVersion, adoptKeychain, configuredBin
	adoptLookPath = func(_ context.Context, kind string) string { return path(kind) }
	adoptVersion = func(context.Context, string) string { return version }
	adoptKeychain = func(context.Context) bool { return keychain }
	configuredBin = func(string) string { return "" }
	t.Cleanup(func() {
		adoptLookPath, adoptVersion, adoptKeychain, configuredBin = oldPath, oldVersion, oldKeychain, oldConfig
	})
}

func TestAdoptInputDetectsUseByBinaryOrLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_CLAUDE_BIN", "")
	t.Setenv("AMUX_CODEX_BIN", "")
	ctx := context.Background()

	withAdoptSeams(t, func(kind string) string {
		if kind == "claude" {
			return "/home/u/.local/bin/claude"
		}
		return ""
	}, "2.1.300", false)
	in := AdoptInput(ctx, "claude")
	if in.OnPath != "/home/u/.local/bin/claude" || in.PathVersion != "2.1.300" || !in.Used() {
		t.Fatalf("claude on PATH: %+v", in)
	}
	if in := AdoptInput(ctx, "codex"); in.Used() {
		t.Fatalf("codex neither installed nor logged in counted as used: %+v", in)
	}
	// Logged in to codex without a binary on PATH still counts as using it.
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if in := AdoptInput(ctx, "codex"); in.LoggedIn == "" || !in.Used() {
		t.Fatalf("codex login not detected: %+v", in)
	}
	// An explicit override short-circuits detection.
	t.Setenv("AMUX_CLAUDE_BIN", "/opt/claude")
	if in := AdoptInput(ctx, "claude"); in.EnvBin != "/opt/claude" || in.OnPath != "" {
		t.Fatalf("override: %+v", in)
	}
}

func TestConversationSavedNeedsEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	dir := t.TempDir()
	root, err := hostprep.OpenSession(dir)
	if err != nil {
		t.Skipf("session root: %v", err)
	}
	defer root.Close()
	for _, s := range []store.Session{
		{ID: "a1", Agent: "claude", Dir: dir},                   // no pinned conversation
		{ID: "a2", Agent: "claude", Dir: dir, ClaudeID: "conv"}, // pinned, never ran a turn
		{ID: "a3", Agent: "codex", Dir: dir, ClaudeID: "conv"},
		{ID: "a4", Agent: "codex", Dir: dir},
		{ID: "a5", Agent: "hermes", Dir: dir},
	} {
		if ok, why := ConversationSaved(root, s, []string{dir}); ok || why == "" {
			t.Errorf("%s (%s): ConversationSaved = %v %q; want false with a reason", s.ID, s.Agent, ok, why)
		}
	}
}
