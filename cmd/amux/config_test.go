package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"amux/internal/core"
)

func TestConfigControlRoundTripPreservesOtherSettings(t *testing.T) {
	sandboxCLI(t)
	t.Setenv("AMUX_CODEX_CONTROL", "pty") // get/ls describe disk, not this override.
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	initial := `{"keys":{"focus-rail":"ctrl+g","unknown-future-key":"alt+f"},"codex":{"future":{"number":9007199254740993}},"extra":{"keep":true}}`
	if err := os.WriteFile(core.ConfigPath(), []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(want string, args ...string) {
		t.Helper()
		out, err := captureOutput(t, func() error { return cmdConfig(args) })
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("config %v = %q, %v; want %q", args, out, err, want)
		}
	}
	run("daemon restart", "set", "codex.control", "app-server")
	run("app-server\n", "get", "codex.control")
	// Keymap reports unknown actions on load, but writes preserve them.
	run("keys.focus-agent", "set", "keys.focus-agent", "ctrl+l")
	run("app-server\n", "get", "codex.control")
	run("default", "unset", "keys.focus-agent")
	run("default", "unset", "codex.control")
	run("pty\n", "get", "codex.control")
	var want, got map[string]any
	if err := json.Unmarshal([]byte(initial), &want); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(core.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// Compare raw JSON as well, so large numbers cannot be silently rounded.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(top["codex"]), "9007199254740993") {
		t.Fatalf("unknown number changed: %s", b)
	}
	w, _ := json.Marshal(want)
	g, _ := json.Marshal(got)
	if string(w) != string(g) {
		t.Fatalf("roundtrip lost config: %s -> %s", initial, b)
	}
}

func TestConfigControlDefaultsListAndValidation(t *testing.T) {
	sandboxCLI(t)
	t.Setenv("AMUX_CODEX_CONTROL", "app-server")
	for _, args := range [][]string{{"get", "codex.control"}, {"ls"}} {
		out, err := captureOutput(t, func() error { return cmdConfig(args) })
		if err != nil || !strings.Contains(out, "pty") {
			t.Fatalf("%v: %s, %v", args, out, err)
		}
	}
	for _, value := range []string{"", "false", "APP-SERVER", "app-servre"} {
		if err := cmdConfig([]string{"set", "codex.control", value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := cmdConfig([]string{"set", "codex.control", "pty"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureOutput(t, func() error { return cmdConfig([]string{"ls"}) })
	if err != nil || !strings.Contains(out, "codex.control pty  (saved;") {
		t.Fatalf("ls: %s, %v", out, err)
	}
}
