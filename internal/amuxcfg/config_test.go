package amuxcfg

import (
	"os"
	"testing"

	"amux/internal/core"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(ControlEnv, "")
	if err := os.Unsetenv(ControlEnv); err != nil {
		t.Fatal(err)
	}
}

func TestControlPrecedence(t *testing.T) {
	for _, saved := range []string{"", PTY, AppServer} {
		for _, override := range []struct {
			name, value      string
			present, warning bool
		}{
			{name: "absent"},
			{name: "empty", present: true},
			{name: "whitespace", value: " \t ", present: true},
			{name: "enable", value: AppServer, present: true},
			{name: "normalized", value: " App-Server\t", present: true},
			{name: "disable", value: PTY, present: true},
			{name: "normalized-disable", value: " PTY ", present: true},
			{name: "legacy-off", value: "0", present: true, warning: true},
			{name: "legacy-false", value: "false", present: true, warning: true},
			{name: "invalid", value: "exec-json", present: true, warning: true},
		} {
			t.Run(saved+"/"+override.name, func(t *testing.T) {
				isolate(t)
				if saved != "" {
					if err := SetCodexControl(saved); err != nil {
						t.Fatal(err)
					}
				}
				if override.present {
					t.Setenv(ControlEnv, override.value)
				}
				c, err := ResolveCodexControl()
				if err != nil {
					t.Fatal(err)
				}
				want, source := PTY, "default"
				if saved != "" {
					want, source = saved, "config"
				}
				switch override.name {
				case "enable", "normalized":
					want, source = AppServer, "environment"
				case "disable", "normalized-disable", "legacy-off", "legacy-false", "invalid":
					want, source = PTY, "environment"
				}
				if c.Effective != want || c.Source != source || c.Persisted != saved || c.Override != override.value || c.OverrideSet != override.present || (c.Warning != "") != override.warning {
					t.Fatalf("got %+v; want effective=%s source=%s warning=%t", c, want, source, override.warning)
				}
			})
		}
	}
}

func TestMalformedPersistentControlRejectedEvenWithOverride(t *testing.T) {
	for _, content := range []string{
		`{`, `null`, `[]`, `{"codex":null}`, `{"codex":"app-server"}`,
		`{"codex":{"control":null}}`, `{"codex":{"control":true}}`,
		`{"codex":{"control":""}}`, `{"codex":{"control":"app-servre"}}`,
		`{"codex":{"control":"APP-SERVER"}}`,
	} {
		t.Run(content, func(t *testing.T) {
			isolate(t)
			if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(core.ConfigPath(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, override := range []string{"", PTY, AppServer} {
				t.Setenv(ControlEnv, override)
				if _, err := ResolveCodexControl(); err == nil {
					t.Fatalf("accepted %s with env %q", content, override)
				}
			}
			b, err := os.ReadFile(core.ConfigPath())
			if err != nil || string(b) != content {
				t.Fatalf("invalid config changed: %q, %v", b, err)
			}
		})
	}
}

func TestControlRepairAndWriteRejection(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":false,"future":42},"extra":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetCodexControl("invalid"); err == nil {
		t.Fatal("accepted invalid value")
	}
	if err := SetCodexControl(AppServer); err != nil {
		t.Fatal(err)
	}
	if got, err := CodexControl(); err != nil || got != AppServer {
		t.Fatalf("repair = %q, %v", got, err)
	}
	if err := SetCodexControl(""); err != nil {
		t.Fatal(err)
	}
	top, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	codex, err := codexSection(top)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codex["control"]; ok {
		t.Fatal("unset retained control")
	}
	if string(codex["future"]) != "42" || string(top["extra"]) != "true" {
		t.Fatalf("lost unknown settings: %v", top)
	}
}
