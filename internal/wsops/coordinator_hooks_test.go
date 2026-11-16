package wsops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/store"
)

// hookCommands returns every hook command registered for event in the
// settings.local.json of launch dir dir.
func hookCommands(t *testing.T, dir, event string) []string {
	t.Helper()
	b, err := os.ReadFile(claudecfg.ProjectSettingsLocalPath(dir))
	if err != nil {
		t.Fatalf("launch dir %s has no settings.local.json: %v", dir, err)
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range root.Hooks[event] {
		for _, h := range g.Hooks {
			out = append(out, h.Command)
		}
	}
	return out
}

func hasCommand(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

// TestAgentCommandInstallsCoordinatorGoalHooks pins how a Claude coordinator
// gets amux's goal hooks: every launch and every resume, through
// AgentCommand → PrepareLaunch, writes them into the launch dir's
// .claude/settings.local.json (not the private CLAUDE_CONFIG_DIR, and not a
// --settings flag), pointed at the session's amux binary. A coordinator whose
// file lost them is repaired on its next start, and the user's template config
// is never touched. An ordinary member gets the status hooks only.
func TestAgentCommandInstallsCoordinatorGoalHooks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	user := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", user)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "off")
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	template := `{"theme":"dark"}`
	if err := os.WriteFile(filepath.Join(user, "settings.json"), []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := store.CoordinatorDir("wg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "44444444-4444-4444-8444-444444444444"
	s := store.Session{ID: "wg", Agent: "claude", Scope: store.ScopeWork, Dir: dir, ClaudeID: uuid, Prompt: "do the task"}
	if s.Role() != store.RoleCoordinator {
		t.Fatalf("role = %q, want coordinator", s.Role())
	}
	bin := core.SessionBinPath(s.ID)
	stop, prompt := bin+" agent goal stop --hook", bin+" agent goal prompt --hook"

	check := func(stage string, wantResume bool) {
		t.Helper()
		launch, _, argv, err := AgentCommand(s)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if launch != dir {
			t.Fatalf("%s: launch dir = %q, want %q", stage, launch, dir)
		}
		if hasFlag(argv, "--resume") != wantResume {
			t.Fatalf("%s: argv = %v, want resume=%v", stage, argv, wantResume)
		}
		if got := hookCommands(t, launch, "Stop"); !hasCommand(got, stop) {
			t.Errorf("%s: Stop hooks = %v, want %q", stage, got, stop)
		}
		if got := hookCommands(t, launch, "UserPromptSubmit"); !hasCommand(got, prompt) {
			t.Errorf("%s: UserPromptSubmit hooks = %v, want %q", stage, got, prompt)
		}
	}

	check("fresh launch", false)

	// An existing coordinator whose settings.local.json lost amux's hooks (or
	// predates goal mode) is repaired on its next resume; unrelated keys survive.
	if err := os.WriteFile(claudecfg.ProjectSettingsLocalPath(dir), []byte(`{"keep":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, munge(dir), uuid)
	check("resume", true)
	if b, _ := os.ReadFile(claudecfg.ProjectSettingsLocalPath(dir)); !strings.Contains(string(b), `"keep"`) {
		t.Errorf("repair dropped unrelated settings: %s", b)
	}

	if b, err := os.ReadFile(filepath.Join(user, "settings.json")); err != nil || string(b) != template {
		t.Errorf("user template settings.json changed: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(user, "settings.local.json")); err == nil {
		t.Error("hooks must not be written into the user's template config")
	}

	// A workgroup member runs Claude's native /goal, not amux's goal hooks.
	memberDir := store.AgentDir("wg", "m1")
	if err := os.MkdirAll(memberDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := store.Session{ID: "m1", RootID: "wg", Agent: "claude", Dir: memberDir, ClaudeID: "55555555-5555-4555-8555-555555555555"}
	if _, _, _, err := AgentCommand(m); err != nil {
		t.Fatal(err)
	}
	if got := hookCommands(t, memberDir, "Stop"); hasCommand(got, bin+" agent goal stop --hook") {
		t.Errorf("member got the coordinator goal Stop hook: %v", got)
	}
}
