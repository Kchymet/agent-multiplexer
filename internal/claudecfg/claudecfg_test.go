package claudecfg

import (
	"encoding/json"
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

// TestInstallHooks verifies the status hooks are written for each event, point
// at the given binary, are idempotent (no stacking on reinstall), and preserve
// the user's own hooks on the same event.
func TestInstallHooks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := SettingsPath()

	// Pre-existing user hook on an event we also manage; it must survive.
	seed := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/my-own-thing"}]}]}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InstallHooks("/opt/amux"); err != nil {
		t.Fatal(err)
	}
	if err := InstallHooks("/opt/amux"); err != nil { // reinstall: must not stack
		t.Fatal(err)
	}

	hooks := readHooks(t, path)
	for _, he := range hookEvents {
		groups, _ := hooks[he.event].([]any)
		amux := 0
		for _, g := range groups {
			if cmd := groupCommand(g); cmd == "/opt/amux hook "+he.state {
				amux++
			}
		}
		if amux != 1 {
			t.Errorf("event %s: got %d amux hook groups, want exactly 1", he.event, amux)
		}
	}

	// The user's own Stop hook must still be present alongside ours.
	var foundUser bool
	for _, g := range hooks["Stop"].([]any) {
		if groupCommand(g) == "/usr/bin/my-own-thing" {
			foundUser = true
		}
	}
	if !foundUser {
		t.Error("user's existing Stop hook was clobbered")
	}
}

func readHooks(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	return hooks
}

// groupCommand returns the first command string inside a hook group, or "".
func groupCommand(g any) string {
	m, _ := g.(map[string]any)
	hs, _ := m["hooks"].([]any)
	if len(hs) == 0 {
		return ""
	}
	hm, _ := hs[0].(map[string]any)
	cmd, _ := hm["command"].(string)
	return cmd
}

// TestPreferredModel covers the rational-default model reader: a configured
// model is returned (trimmed), and every degenerate case (missing key, missing
// file, malformed JSON) yields "" so callers fall back to Claude's own default.
func TestPreferredModel(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" means write no file at all
		want    string
	}{
		{"configured", `{"model":"opus"}`, "opus"},
		{"trimmed", `{"model":"  sonnet  "}`, "sonnet"},
		{"empty value", `{"model":""}`, ""},
		{"key absent", `{"theme":"dark"}`, ""},
		{"missing file", "", ""},
		{"malformed json", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := PreferredModel(); got != tc.want {
				t.Errorf("PreferredModel() = %q, want %q", got, tc.want)
			}
		})
	}
}
