package codexcfg

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/cfghome"
)

// TestTemplateSeed pins a Codex agent home's seed: config.toml and AGENTS.md are
// copied, sessions are not, auth.json is a symlink to the user's; the trust table
// amux writes into the copy at launch is not drift, a model change is.
func TestTemplateSeed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	user := filepath.Join(root, "home", ".codex")
	t.Setenv("CODEX_HOME", user)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(user, "sessions", "2026"), 0o755))
	must(os.WriteFile(filepath.Join(user, ConfigFile), []byte("model = \"gpt-5.5\"\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, "AGENTS.md"), []byte("# rules\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, AuthFile), []byte("{}"), 0o600))
	must(os.WriteFile(filepath.Join(user, "sessions", "2026", "r.jsonl"), []byte("{}"), 0o644))

	agentDir := filepath.Join(root, "agent")
	sp := Template("a1", AgentHome(agentDir))
	if sp.Env != "CODEX_HOME" || sp.Dir != filepath.Join(agentDir, ".amux", "codex") {
		t.Fatalf("spec = %+v", sp)
	}
	if fresh, err := cfghome.Seed(sp); err != nil || !fresh {
		t.Fatalf("Seed = %v, %v", fresh, err)
	}
	home := At(sp.Dir)
	if home.PreferredModel() != "gpt-5.5" {
		t.Error("config.toml not seeded")
	}
	if _, err := os.Stat(filepath.Join(sp.Dir, "AGENTS.md")); err != nil {
		t.Error("AGENTS.md not seeded")
	}
	if _, err := os.Stat(filepath.Join(sp.Dir, "sessions")); !os.IsNotExist(err) {
		t.Error("sessions/ must not be copied")
	}
	if target, err := os.Readlink(home.AuthPath()); err != nil || target != UserHome().AuthPath() {
		t.Errorf("auth.json should link to the user's: %q %v", target, err)
	}

	must(home.TrustDir(agentDir)) // what launch does
	if ch, _ := cfghome.Scan(sp); len(ch) != 0 {
		t.Fatalf("launch-time trust reported as drift: %v", ch)
	}
	must(os.WriteFile(home.ConfigPath(), []byte("model = \"gpt-5.4\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"), 0o644))
	ch, _ := cfghome.Scan(sp)
	if len(ch) != 1 || ch[0].Rel != ConfigFile || ch[0].Status != cfghome.AgentChanged {
		t.Fatalf("model change should read as an agent edit, got %v", ch)
	}
}
