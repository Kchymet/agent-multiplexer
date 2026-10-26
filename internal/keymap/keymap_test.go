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
		"Alt+PageUp":    "alt+pgup",
		"Alt+PageDown":  "alt+pgdown",
		"alt+pgdn":      "alt+pgdown",
		"Alt+End":       "alt+end",
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
		"pgup", "pgdown", "end", "shift+up", "shift+enter",
		"shift+pgup", "shift+pgdown",
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

func TestHistoryBindingsRoundTrip(t *testing.T) {
	configDir(t)
	for _, tc := range []struct{ action, chord string }{
		{ScrollUp, "alt+pgup"},
		{ScrollDown, "alt+pgdown"},
		{ScrollLive, "alt+end"},
		{ScrollHalfUp, "ctrl+u"},
		{ScrollHalfDown, "ctrl+d"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			km, err := Load()
			if err != nil || km.Chord(tc.action) != tc.chord || km.Action(tc.chord) != tc.action {
				t.Fatalf("default %s = %q, %v", tc.action, km.Chord(tc.action), err)
			}
			if _, err := Set(tc.action, "alt+h"); err == nil {
				t.Fatal("accepted collision with focus-rail")
			}
			if _, err := Set(tc.action, tc.chord); err != nil {
				t.Fatal(err)
			}
			if _, err := Set(Quit, tc.chord); err == nil {
				t.Fatal("accepted collision with history default")
			}
			if _, err := Set(tc.action, "ctrl+g"); err != nil {
				t.Fatal(err)
			}
			km, err = Load()
			if err != nil || km.Action("ctrl+g") != tc.action || km.Action(tc.chord) != "" {
				t.Fatalf("rebound %s: %v, %v", tc.action, km.List(), err)
			}
			if err := Unset(tc.action); err != nil {
				t.Fatal(err)
			}
			km, err = Load()
			if err != nil || km.Action(tc.chord) != tc.action {
				t.Fatalf("restored %s: %v, %v", tc.action, km.List(), err)
			}
			if _, err := Set(tc.action, tc.chord); err != nil {
				t.Fatalf("cannot persist default chord: %v", err)
			}
		})
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

func TestHalfPageDefaultsPreserveExistingBindings(t *testing.T) {
	configDir(t)
	if err := os.MkdirAll(core.ConfigDir(), 0700); err != nil {
		t.Fatal(err)
	}
	// Simulate a config saved before the half-page actions existed.
	original := []byte(`{"keys":{"scroll-up":"Ctrl+U","scroll-down":"Ctrl+D"}}`)
	if err := os.WriteFile(core.ConfigPath(), original, 0600); err != nil {
		t.Fatal(err)
	}
	km, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if km.Action("ctrl+u") != ScrollUp || km.Action("ctrl+d") != ScrollDown || km.Action("") != "" {
			t.Fatal("configured bindings lost priority")
		}
	}
	if km.Chord(ScrollHalfUp) != "" || km.Chord(ScrollHalfDown) != "" {
		t.Fatal("new defaults still collide")
	}
	saved, _ := os.ReadFile(core.ConfigPath())
	if string(saved) != string(original) {
		t.Fatal("loading rewrote existing preferences")
	}
	if _, err := Set(ScrollHalfUp, "ctrl+u"); err == nil {
		t.Fatal("accepted two explicit bindings for Ctrl+U")
	}
	if _, err := Set(ScrollUp, "f5"); err != nil {
		t.Fatal(err)
	}
	km, err = Load()
	if err != nil || km.Action("ctrl+u") != ScrollHalfUp || km.Action("f5") != ScrollUp || km.Action("ctrl+d") != ScrollDown {
		t.Fatalf("default not restored: %+v, %v", km.List(), err)
	}
}
