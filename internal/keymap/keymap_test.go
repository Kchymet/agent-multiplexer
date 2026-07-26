package keymap

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
)

// configDir points the package at a throwaway config dir for one test.
func configDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "amux")
}

func TestLoadDefaultsWithoutFile(t *testing.T) {
	configDir(t)
	km, err := Load()
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}
	if got := km.Chord(FocusRail); got != "alt+h" {
		t.Fatalf("default focus-rail = %q, want alt+h", got)
	}
	if got := km.Action("alt+l"); got != FocusAgent {
		t.Fatalf("Action(alt+l) = %q, want %s", got, FocusAgent)
	}
}

func TestZeroValueIsDefaults(t *testing.T) {
	var km Keymap
	if got := km.Action("alt+q"); got != Quit {
		t.Fatalf("zero keymap Action(alt+q) = %q, want %s", got, Quit)
	}
}

func TestSetLoadRoundTrip(t *testing.T) {
	configDir(t)
	chord, err := Set(FocusRail, "Ctrl+G")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if chord != "ctrl+g" {
		t.Fatalf("Set normalized to %q, want ctrl+g", chord)
	}
	km, err := Load()
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if got := km.Chord(FocusRail); got != "ctrl+g" {
		t.Fatalf("focus-rail after Set = %q, want ctrl+g", got)
	}
	// The old default chord must no longer resolve, and the new one must.
	if got := km.Action("alt+h"); got != "" {
		t.Fatalf("Action(alt+h) after rebind = %q, want unbound", got)
	}
	if got := km.Action("ctrl+g"); got != FocusRail {
		t.Fatalf("Action(ctrl+g) = %q, want %s", got, FocusRail)
	}
}

func TestUnsetRestoresDefault(t *testing.T) {
	configDir(t)
	if _, err := Set(Quit, "ctrl+x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := Unset(Quit); err != nil {
		t.Fatalf("Unset: %v", err)
	}
	km, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := km.Chord(Quit); got != "alt+q" {
		t.Fatalf("quit after Unset = %q, want alt+q", got)
	}
}

func TestSetRejectsCollisions(t *testing.T) {
	configDir(t)
	// alt+a is the toggle-focus default: binding quit onto it must fail.
	if _, err := Set(Quit, "alt+a"); err == nil {
		t.Fatal("Set(quit, alt+a) succeeded, want collision error")
	}
	// …but rebinding an action onto its own current chord is fine.
	if _, err := Set(Quit, "alt+q"); err != nil {
		t.Fatalf("Set(quit, alt+q): %v", err)
	}
}

func TestSetRejectsUnknownAction(t *testing.T) {
	configDir(t)
	if _, err := Set("open-portal", "alt+p"); err == nil {
		t.Fatal("Set with unknown action succeeded")
	}
}

func TestNormalize(t *testing.T) {
	good := map[string]string{
		"alt+h":         "alt+h",
		"Option + L":    "alt+l",
		"opt+1":         "alt+1",
		"meta+q":        "alt+q",
		"CTRL+G":        "ctrl+g",
		"control+alt+t": "ctrl+alt+t",
		"ctrl+c":        "ctrl+c",
		"alt+Escape":    "alt+esc",
		"f5":            "f5",
		"shift+f5":      "shift+f5",
	}
	for in, want := range good {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	bad := []string{
		"",        // empty
		"alt",     // no base key
		"q",       // no modifier: would be stolen from the agent
		"shift+q", // shift alone doesn't disambiguate a rune either
		"alt+h+l", // two base keys
		"alt+bogus",
		"f99",
	}
	for _, in := range bad {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want error", in, got)
		}
	}
}

func TestLoadKeepsGoingOnBadEntries(t *testing.T) {
	dir := configDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"keys": {"focus-rail": "ctrl+g", "quit": "not a key", "open-portal": "alt+p"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	km, err := Load()
	if err == nil {
		t.Fatal("Load with bad entries returned no error")
	}
	// The valid override applies; the broken ones fall back to defaults.
	if got := km.Chord(FocusRail); got != "ctrl+g" {
		t.Fatalf("focus-rail = %q, want ctrl+g", got)
	}
	if got := km.Chord(Quit); got != "alt+q" {
		t.Fatalf("quit fell back to %q, want alt+q", got)
	}
	if !strings.Contains(err.Error(), "open-portal") || !strings.Contains(err.Error(), "quit") {
		t.Fatalf("error %q does not name the bad entries", err)
	}
}

func TestWritePreservesForeignFields(t *testing.T) {
	dir := configDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"future": {"nested": true}, "keys": {"quit": "ctrl+x"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(FocusRail, "ctrl+g"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	b, err := os.ReadFile(core.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("rewritten config is not valid JSON: %v\n%s", err, b)
	}
	if top["future"] == nil {
		t.Fatalf("Set dropped a foreign top-level field:\n%s", b)
	}
	var keys map[string]string
	if err := json.Unmarshal(top["keys"], &keys); err != nil {
		t.Fatal(err)
	}
	if keys["quit"] != "ctrl+x" || keys["focus-rail"] != "ctrl+g" {
		t.Fatalf("keys after Set = %v", keys)
	}
}
