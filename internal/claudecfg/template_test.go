package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/cfghome"
)

// TestTemplateSeed pins what a fresh agent home is seeded with from the user's
// Claude home: settings (with absolute references to the template rewritten to
// the copy), memory, commands, and .claude.json minus its per-project trust
// table — while transcripts and history stay out, and the credentials file is a
// symlink to the user's rather than a copy.
func TestTemplateSeed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	user := filepath.Join(root, "home", ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", user)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(user, "commands"), 0o755))
	must(os.MkdirAll(filepath.Join(user, "projects", "-x"), 0o755))
	must(os.WriteFile(filepath.Join(user, "settings.json"),
		[]byte(`{"statusLine":{"command":"bash `+user+`/statusline-command.sh"},"theme":"dark"}`), 0o600))
	must(os.WriteFile(filepath.Join(user, "statusline-command.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(os.WriteFile(filepath.Join(user, "CLAUDE.md"), []byte("# memory\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, "commands", "go.md"), []byte("go"), 0o644))
	must(os.WriteFile(filepath.Join(user, "history.jsonl"), []byte("{}\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, "projects", "-x", "t.jsonl"), []byte("{}\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, CredentialsFile), []byte(`{"token":"x"}`), 0o600))
	must(os.WriteFile(User().ConfigPath(),
		[]byte(`{"hasCompletedOnboarding":true,"numStartups":7,"mcpServers":{"a":{}},"projects":{"/home/u":{"hasTrustDialogAccepted":true}}}`), 0o600))

	agentDir := filepath.Join(root, "agent")
	sp := Template("a1", AgentHome(agentDir))
	if sp.Env != "CLAUDE_CONFIG_DIR" || sp.Dir != filepath.Join(agentDir, ".amux", "claude") || sp.Template != user {
		t.Fatalf("spec = %+v", sp)
	}
	fresh, err := cfghome.Seed(sp)
	if err != nil || !fresh {
		t.Fatalf("Seed = %v, %v", fresh, err)
	}
	home := At(sp.Dir)

	// settings.json: copied, with the template path rewritten to the copy.
	b, _ := os.ReadFile(home.SettingsPath())
	if !strings.Contains(string(b), sp.Dir+"/statusline-command.sh") || strings.Contains(string(b), user) {
		t.Errorf("settings.json should reference the copy's own script: %s", b)
	}
	for _, rel := range []string{"statusline-command.sh", "CLAUDE.md", "commands/go.md"} {
		if _, err := os.Stat(filepath.Join(sp.Dir, rel)); err != nil {
			t.Errorf("%s not seeded", rel)
		}
	}
	for _, rel := range []string{"history.jsonl", "projects"} {
		if _, err := os.Stat(filepath.Join(sp.Dir, rel)); !os.IsNotExist(err) {
			t.Errorf("state %s must not be copied", rel)
		}
	}
	// .claude.json: inside the copy, onboarding/mcp kept, projects dropped.
	b, _ = os.ReadFile(home.ConfigPath())
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf(".claude.json unreadable: %v", err)
	}
	if cfg["hasCompletedOnboarding"] != true || cfg["mcpServers"] == nil {
		t.Errorf(".claude.json lost config/onboarding: %v", cfg)
	}
	if _, has := cfg["projects"]; has {
		t.Errorf(".claude.json should be seeded without the user's projects table")
	}
	// Credentials: a symlink to the user's file, never a copy.
	if target, err := os.Readlink(home.CredentialsPath()); err != nil || target != User().CredentialsPath() {
		t.Errorf("credentials should link to the user's file: %q %v", target, err)
	}
	if binds := cfghome.Binds(sp); len(binds) != 1 || binds[0][1] != User().CredentialsPath() {
		t.Errorf("Binds = %v, want just the credentials file at its own path", binds)
	}

	// A fresh copy has no drift — including the rewritten settings paths.
	if ch, _ := cfghome.Scan(sp); len(ch) != 0 {
		t.Fatalf("fresh copy reports drift: %v", ch)
	}
	// Claude's own churn in .claude.json (counters, trust granted at launch) is
	// not an edit; a new MCP server is.
	must(home.TrustDir(agentDir))
	b, _ = os.ReadFile(home.ConfigPath())
	must(os.WriteFile(home.ConfigPath(), []byte(strings.Replace(string(b), `"numStartups": 7`, `"numStartups": 8`, 1)), 0o600))
	if ch, _ := cfghome.Scan(sp); len(ch) != 0 {
		t.Fatalf("bookkeeping churn reported as drift: %v", ch)
	}
	must(os.WriteFile(home.ConfigPath(), []byte(`{"numStartups":9,"mcpServers":{"a":{},"b":{"command":"x"}}}`), 0o600))
	ch, _ := cfghome.Scan(sp)
	if len(ch) != 1 || ch[0].Rel != ".claude.json" || ch[0].Status != cfghome.AgentChanged {
		t.Fatalf("new MCP server should read as an agent edit, got %v", ch)
	}
	// Promote merges just the config keys into the user's file: their state stays.
	must(cfghome.Promote(sp, ".claude.json"))
	b, _ = os.ReadFile(User().ConfigPath())
	var userCfg map[string]any
	must(json.Unmarshal(b, &userCfg))
	if userCfg["numStartups"] != float64(7) || userCfg["projects"] == nil {
		t.Errorf("Promote clobbered the user's .claude.json state: %v", userCfg)
	}
	if mcp, _ := userCfg["mcpServers"].(map[string]any); mcp["b"] == nil {
		t.Errorf("Promote should have added the MCP server: %v", userCfg["mcpServers"])
	}
	if ch, _ := cfghome.Scan(sp); len(ch) != 0 {
		t.Fatalf("after promote: %v", ch)
	}
}
