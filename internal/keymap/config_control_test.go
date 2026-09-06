package keymap

import (
	"encoding/json"
	"os"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/core"
)

func TestControlAndKeyWritesPreserveOtherSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configDir(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"keys":{"focus-rail":"ctrl+g"},"codex":{"future":9007199254740993},"future":{"keep":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(wantControl, wantChord string) {
		t.Helper()
		got, err := amuxcfg.CodexControl()
		if err != nil || got != wantControl {
			t.Fatalf("control = %q, %v", got, err)
		}
		km, err := Load()
		if err != nil || km.Chord(FocusRail) != wantChord {
			t.Fatalf("chord = %q, %v", km.Chord(FocusRail), err)
		}
		top, err := amuxcfg.Read()
		if err != nil {
			t.Fatal(err)
		}
		var codex, future map[string]json.RawMessage
		if err := json.Unmarshal(top["codex"], &codex); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(top["future"], &future); err != nil {
			t.Fatal(err)
		}
		if string(codex["future"]) != "9007199254740993" || string(future["keep"]) != "true" {
			t.Fatalf("unknown settings changed: %v", top)
		}
	}
	if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
		t.Fatal(err)
	}
	check(amuxcfg.AppServer, "ctrl+g")
	if _, err := Set(FocusRail, "ctrl+h"); err != nil {
		t.Fatal(err)
	}
	check(amuxcfg.AppServer, "ctrl+h")
	if err := Unset(FocusRail); err != nil {
		t.Fatal(err)
	}
	check(amuxcfg.AppServer, "alt+h")
	if err := amuxcfg.SetCodexControl(""); err != nil {
		t.Fatal(err)
	}
	check("", "alt+h")
}
