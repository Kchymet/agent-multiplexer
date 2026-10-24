package codexcfg

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/cfghome"
)

// TestTemplateSeed pins a Codex agent home's seed: config.toml and AGENTS.md are
// copied, sessions are not, auth.json is a symlink to the user's; the trust table
// amux writes into the copy at launch is not drift, a model change is.
func TestTemplateSeed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	user := filepath.Join(root, "home", ".codex")
	t.Setenv("CODEX_HOME", user)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Account sharing is the explicit legacy host selection.
	must(amuxcfg.SetAuthMode("codex", amuxcfg.AuthHost))
	must(os.MkdirAll(filepath.Join(user, "sessions", "2026"), 0o755))
	config := "model = \"gpt-5.5\"\n\n[mcp_servers.linear]\nurl = \"https://mcp.linear.app/mcp\"\n"
	must(os.WriteFile(filepath.Join(user, ConfigFile), []byte(config), 0o644))
	must(os.WriteFile(filepath.Join(user, "AGENTS.md"), []byte("# rules\n"), 0o644))
	must(os.WriteFile(filepath.Join(user, AuthFile), []byte("{}"), 0o600))
	must(os.WriteFile(filepath.Join(user, MCPCredentialsFile), []byte(`{"linear":"test-token"}`), 0o600))
	must(os.MkdirAll(filepath.Join(user, "mcp-oauth-locks"), 0o700))
	must(os.WriteFile(filepath.Join(user, "sessions", "2026", "r.jsonl"), []byte("{}"), 0o644))

	agentDir := filepath.Join(root, "agent")
	must(os.MkdirAll(agentDir, 0o755))
	sp := Template("a1", agentDir)
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
	if b, err := os.ReadFile(home.ConfigPath()); err != nil || string(b) != config {
		t.Fatalf("MCP configuration not inherited: %q, %v", b, err)
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
	credential := filepath.Join(sp.Dir, MCPCredentialsFile)
	local, err := os.Lstat(credential)
	must(err)
	source, err := os.Stat(filepath.Join(user, MCPCredentialsFile))
	must(err)
	if !local.Mode().IsRegular() || !os.SameFile(local, source) {
		t.Fatal("MCP credential must share the user's regular file (Codex writes with O_NOFOLLOW)")
	}
	if local.Mode().Perm() != 0o600 {
		t.Fatal("MCP credential permissions changed")
	}
	// Refreshes update the shared token, and do not become promotable config.
	f, err := os.OpenFile(credential, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0)
	must(err)
	_, err = f.WriteString(`{"linear":"refreshed-test-token"}`)
	must(err)
	must(f.Close())
	if b, err := os.ReadFile(filepath.Join(user, MCPCredentialsFile)); err != nil || !strings.Contains(string(b), "refreshed-test-token") {
		t.Fatalf("MCP refresh not shared: %q, %v", b, err)
	}
	if target, err := os.Readlink(filepath.Join(sp.Dir, "mcp-oauth-locks")); err != nil || target != filepath.Join(user, "mcp-oauth-locks") {
		t.Fatalf("MCP refresh locks not shared: %q, %v", target, err)
	}
	if err := cfghome.Promote(sp, MCPCredentialsFile); err == nil {
		t.Fatal("MCP tokens must never be promoted as configuration")
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

// TestCentralTemplateSharesNoAccountLogin pins central and environment Codex
// homes: no auth.json (a legacy link is unlinked without touching the host
// login), MCP credentials still shared, and only central clears API keys.
func TestCentralTemplateSharesNoAccountLogin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	user := filepath.Join(root, "home", ".codex")
	t.Setenv("CODEX_HOME", user)
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(user, 0o700))
	must(os.WriteFile(filepath.Join(user, AuthFile), []byte(`{"tokens":"host-login"}`), 0o600))
	must(os.WriteFile(filepath.Join(user, MCPCredentialsFile), []byte(`{"linear":"test-token"}`), 0o600))
	must(amuxcfg.SetAuthMode("codex", amuxcfg.AuthHost))
	agentDir := filepath.Join(root, "agent")
	must(os.MkdirAll(agentDir, 0o755))
	if _, err := cfghome.Seed(Template("a1", agentDir)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{amuxcfg.AuthCentral, amuxcfg.AuthEnvironment} {
		must(amuxcfg.SetAuthMode("codex", mode))
		sp := Template("a1", agentDir)
		if _, err := cfghome.Seed(sp); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(sp.Dir, AuthFile)); !os.IsNotExist(err) {
			t.Fatalf("%s home has auth.json: %v", mode, err)
		}
		for _, b := range cfghome.Binds(sp) {
			if strings.HasSuffix(b[1], AuthFile) {
				t.Fatalf("%s binds the host login: %v", mode, b)
			}
		}
		if _, err := os.Stat(filepath.Join(sp.Dir, MCPCredentialsFile)); err != nil {
			t.Fatalf("%s lost MCP credential sharing: %v", mode, err)
		}
		cleared := strings.Join(sp.EnvEntries(), " ")
		if got := strings.Contains(cleared, "OPENAI_API_KEY="); got != (mode == amuxcfg.AuthCentral) {
			t.Fatalf("%s API key cleared = %v: %s", mode, got, cleared)
		}
	}
	if b, err := os.ReadFile(filepath.Join(user, AuthFile)); err != nil || string(b) != `{"tokens":"host-login"}` {
		t.Fatalf("host login changed: %q, %v", b, err)
	}
}
