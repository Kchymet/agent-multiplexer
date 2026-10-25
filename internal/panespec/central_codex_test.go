package panespec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/accounts"
	"amux/internal/amuxcfg"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/store"
)

// TestCentralCodexAppServerPinsOfficialChatGPTEndpoint: a session's editable
// config.toml must not redirect the central access token, so a central App
// Server pins the endpoint with invocation overrides; host and environment
// modes keep the user's configuration.
func TestCentralCodexAppServerPinsOfficialChatGPTEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CODEX_BIN", "/bin/true")
	useFakeSecureBwrap(t)
	dir := filepath.Join(core.SessionsDir(), "group", "agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a", RootID: "group", Agent: "codex", Dir: dir}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	db.Close()
	pins := []string{`chatgpt_base_url="` + codexcfg.OfficialChatGPTBaseURL + `"`, `model_provider="openai"`, `openai_base_url=""`}
	for mode, want := range map[string]bool{amuxcfg.AuthCentral: true, amuxcfg.AuthHost: false, amuxcfg.AuthEnvironment: false} {
		useCodexAuth(t, mode)
		if err := accounts.Update(func(st *accounts.State) error {
			b := st.Bindings[s.ID]
			b.Source = mode
			b.ProfileID = "codex:" + mode
			b.Revision++
			st.Bindings[s.ID] = b
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_, _, argv, _, err := AppServerCommand(testLaunchSpec(t, s))
		if err != nil {
			t.Fatal(err)
		}
		for _, pin := range pins {
			i := slices.Index(argv, pin)
			if (i >= 0) != want {
				t.Fatalf("%s: pin %s present = %v; argv=%v", mode, pin, i >= 0, argv)
			}
			if want && (i == 0 || argv[i-1] != "-c") {
				t.Fatalf("%s: %s is not a -c override: %v", mode, pin, argv)
			}
		}
		if want && !strings.Contains(strings.Join(argv, " "), "app-server --listen") {
			t.Fatalf("pins displaced the subcommand: %v", argv)
		}
	}
}
