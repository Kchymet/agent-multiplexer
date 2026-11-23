package claudecfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/cfghome"
	"amux/internal/hostprep"
)

func decodeSettings(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings do not parse: %v: %s", err, b)
	}
	return m
}

func environmentOf(m map[string]any) any {
	am, _ := m[AutoModeKey].(map[string]any)
	return am[AutoModeEnvironmentKey]
}

func TestApplyUnattendedSettings(t *testing.T) {
	cases := []struct {
		name, in, mode string
		wantSkip       bool
		wantEnv        []any
	}{
		{"empty file, auto launch", ``, "auto", true, []any{"$defaults"}},
		{"no keys, auto launch", `{"theme":"dark"}`, "auto", true, []any{"$defaults"}},
		{"user entries keep their order, $defaults added", `{"autoMode":{"environment":["Trusted repo: acme"]}}`, "auto",
			true, []any{"$defaults", "Trusted repo: acme"}},
		{"user entries with $defaults untouched", `{"autoMode":{"environment":["x","$defaults"]}}`, "auto",
			true, []any{"x", "$defaults"}},
		{"empty environment answered", `{"autoMode":{"environment":[]}}`, "auto", true, []any{"$defaults"}},
		{"non-auto launch: no opt-in key", `{}`, "acceptEdits", false, []any{"$defaults"}},
		{"no flag, settings default auto", `{"permissions":{"defaultMode":"auto"}}`, "", true, []any{"$defaults"}},
		{"no flag, settings default other", `{"permissions":{"defaultMode":"plan"}}`, "", false, []any{"$defaults"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := ApplyUnattendedSettings([]byte(tc.in), tc.mode)
			m := decodeSettings(t, out)
			if got, _ := m[SkipAutoPermissionPromptKey].(bool); got != tc.wantSkip {
				t.Errorf("%s = %v, want %v: %s", SkipAutoPermissionPromptKey, got, tc.wantSkip, out)
			}
			if got := environmentOf(m); !reflect.DeepEqual(got, tc.wantEnv) {
				t.Errorf("environment = %v, want %v", got, tc.wantEnv)
			}
			// Idempotent: a second pass changes nothing, byte for byte.
			again, changed := ApplyUnattendedSettings(out, tc.mode)
			if changed || !bytes.Equal(again, out) {
				t.Errorf("second apply changed the file:\n%s\n->\n%s", out, again)
			}
		})
	}
}

// TestApplyUnattendedSettingsPreservesOtherKeys: the merge touches only the two
// managed values — never the classifier rules, permissions, hooks, or anything else.
func TestApplyUnattendedSettingsPreservesOtherKeys(t *testing.T) {
	in := `{
  "permissions": {"defaultMode": "auto", "allow": ["Bash(ls)"]},
  "autoMode": {"allow": ["a"], "soft_deny": ["s"], "hard_deny": ["h"]},
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "x"}]}]},
  "cleanupPeriodDays": 30,
  "model": "opus"
}`
	out, changed := ApplyUnattendedSettings([]byte(in), "auto")
	if !changed {
		t.Fatal("expected a change")
	}
	got, want := decodeSettings(t, out), decodeSettings(t, []byte(in))
	want[SkipAutoPermissionPromptKey] = true
	want[AutoModeKey].(map[string]any)[AutoModeEnvironmentKey] = []any{"$defaults"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge disturbed other settings:\n got %v\nwant %v", got, want)
	}
	// Already answered: nothing to do, the file is returned as is.
	done := `{"skipAutoPermissionPrompt":true,"autoMode":{"environment":["$defaults"]},"x":1}`
	if out, changed := ApplyUnattendedSettings([]byte(done), "auto"); changed || string(out) != done {
		t.Fatalf("answered settings rewritten: %s", out)
	}
	// An invalid autoMode is left alone, but the opt-in is still answered.
	if out, _ := ApplyUnattendedSettings([]byte(`{"autoMode":"x"}`), "auto"); !reflect.DeepEqual(decodeSettings(t, out),
		map[string]any{"autoMode": "x", SkipAutoPermissionPromptKey: true}) {
		t.Fatalf("invalid autoMode: %s", out)
	}
	// Malformed JSON is never clobbered.
	if out, changed := ApplyUnattendedSettings([]byte(`{"oops"`), "auto"); changed || string(out) != `{"oops"` {
		t.Fatalf("malformed settings rewritten: %s", out)
	}
}

// unattendedFixture is a user Claude home with settings and state, and an agent
// dir to template it into.
func unattendedFixture(t *testing.T, settings string) (user, agentDir string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv(PermissionModeEnv, "")
	user = filepath.Join(root, "home", ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", user)
	if err := os.MkdirAll(user, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(user, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(User().ConfigPath(), []byte(`{"numStartups":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	agentDir = filepath.Join(root, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return user, agentDir
}

// TestFreshSandboxIsUnattended: a newly seeded agent home already carries the
// pre-answers, and they do not read as drift against a template without them.
func TestFreshSandboxIsUnattended(t *testing.T) {
	_, agentDir := unattendedFixture(t, `{"theme":"dark","autoMode":{"environment":["Org: acme"]}}`)
	sp := Template("a1", agentDir)
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(At(sp.Dir).SettingsPath())
	m := decodeSettings(t, b)
	if m[SkipAutoPermissionPromptKey] != true || m["theme"] != "dark" {
		t.Errorf("seeded settings = %s", b)
	}
	if got := environmentOf(m); !reflect.DeepEqual(got, []any{"$defaults", "Org: acme"}) {
		t.Errorf("seeded environment = %v, want the user's entries plus $defaults", got)
	}
	if ch, err := cfghome.Scan(sp); err != nil || len(ch) != 0 {
		t.Fatalf("fresh sandbox reports drift: %v %v", ch, err)
	}
}

// TestExistingSandboxGetsUnattendedMerge: a home seeded before amux managed the
// keys gets them at launch, keeps every other setting and state key, reports no
// drift for them, and a second launch rewrites nothing.
func TestExistingSandboxGetsUnattendedMerge(t *testing.T) {
	_, agentDir := unattendedFixture(t, `{"theme":"dark"}`)
	sp := Template("a1", agentDir)
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	home := At(sp.Dir)
	// Roll the copy back to what an older amux seeded, with an agent-side state
	// key in .claude.json, and settle the baseline on it.
	legacy := []byte(`{"theme":"dark"}`)
	if err := os.WriteFile(home.SettingsPath(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home.SettingsPath(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home.ConfigPath(), []byte(`{"numStartups":9,"userID":"u"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ch, err := cfghome.Scan(sp); err != nil || len(ch) != 0 {
		t.Fatalf("legacy copy drift: %v %v", ch, err)
	}

	session, err := hostprep.OpenSession(agentDir)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := home.EnsureUnattendedRooted(session, "auto"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(home.SettingsPath())
	m := decodeSettings(t, b)
	if m["theme"] != "dark" || m[SkipAutoPermissionPromptKey] != true ||
		!reflect.DeepEqual(environmentOf(m), []any{"$defaults"}) {
		t.Fatalf("merged settings = %s", b)
	}
	if fi, _ := os.Stat(home.SettingsPath()); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings mode = %v, want the file's own 0600 kept", fi.Mode().Perm())
	}
	cfg := map[string]any{}
	cb, _ := os.ReadFile(home.ConfigPath())
	_ = json.Unmarshal(cb, &cfg)
	if cfg[HasCompletedOnboardingKey] != true || cfg["numStartups"] != float64(9) || cfg["userID"] != "u" {
		t.Fatalf(".claude.json = %s", cb)
	}
	if ch, err := cfghome.Scan(sp); err != nil || len(ch) != 0 {
		t.Fatalf("amux-managed keys read as drift: %v %v", ch, err)
	}

	// Idempotent: a relaunch leaves both files byte-identical.
	before, _ := os.ReadFile(home.SettingsPath())
	if err := home.EnsureUnattendedRooted(session, "auto"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(home.SettingsPath())
	cb2, _ := os.ReadFile(home.ConfigPath())
	if !bytes.Equal(before, after) || !bytes.Equal(cb, cb2) {
		t.Fatalf("second ensure rewrote files:\n%s\n->\n%s", before, after)
	}
}

// TestUnattendedKeysAndDrift: a real edit next to the managed keys still shows
// as drift, and promoting it carries the edit without amux's plumbing.
func TestUnattendedKeysAndDrift(t *testing.T) {
	user, agentDir := unattendedFixture(t, `{"theme":"dark"}`)
	sp := Template("a1", agentDir)
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	home := At(sp.Dir)
	b, _ := os.ReadFile(home.SettingsPath())
	m := decodeSettings(t, b)
	m["theme"] = "light"
	m[AutoModeKey].(map[string]any)[AutoModeEnvironmentKey] = []any{"$defaults", "Trusted bucket: s3://x"}
	edited, _ := json.Marshal(m)
	if err := os.WriteFile(home.SettingsPath(), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	ch, err := cfghome.Scan(sp)
	if err != nil || len(ch) != 1 || ch[0].Rel != "settings.json" || ch[0].Status != cfghome.AgentChanged {
		t.Fatalf("edit drift = %v %v", ch, err)
	}
	if err := cfghome.Promote(sp, "settings.json"); err != nil {
		t.Fatal(err)
	}
	tb, _ := os.ReadFile(filepath.Join(user, "settings.json"))
	tm := decodeSettings(t, tb)
	if _, leaked := tm[SkipAutoPermissionPromptKey]; leaked {
		t.Errorf("promote leaked %s into the user's settings: %s", SkipAutoPermissionPromptKey, tb)
	}
	if tm["theme"] != "light" || !reflect.DeepEqual(environmentOf(tm), []any{"$defaults", "Trusted bucket: s3://x"}) {
		t.Errorf("promoted settings = %s", tb)
	}

	// A bare ["$defaults"] promoted onto a template with no environment is
	// dropped too.
	m[AutoModeKey].(map[string]any)[AutoModeEnvironmentKey] = []any{"$defaults"}
	m["theme"] = "dark"
	if err := os.WriteFile(filepath.Join(user, "settings.json"), []byte(`{"theme":"light"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	edited, _ = json.Marshal(m)
	out, err := mergeSettings(sp, []byte(`{"theme":"light"}`), edited)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeSettings(t, out); !reflect.DeepEqual(got, map[string]any{"theme": "dark"}) {
		t.Errorf("merge = %s, want only the agent's edit", out)
	}
}

// TestNormalizeSettings pins what drift compares for settings.json.
func TestNormalizeSettings(t *testing.T) {
	same := [][2]string{
		{`{"theme":"dark"}`, `{"skipAutoPermissionPrompt":true,"autoMode":{"environment":["$defaults"]},"theme":"dark"}`},
		{`{"autoMode":{"environment":["x"]}}`, `{"autoMode":{"environment":["$defaults","x"]}}`},
		{`{"a":1,"b":2}`, "{\n  \"b\": 2,\n  \"a\": 1\n}"},
	}
	for _, p := range same {
		if a, b := normalizeSettings([]byte(p[0])), normalizeSettings([]byte(p[1])); !bytes.Equal(a, b) {
			t.Errorf("%s and %s should compare equal:\n%s\n%s", p[0], p[1], a, b)
		}
	}
	differ := [][2]string{
		{`{"autoMode":{"environment":["x"]}}`, `{"autoMode":{"environment":["y"]}}`},
		{`{}`, `{"skipAutoPermissionPrompt":false}`},
		{`{}`, `{"autoMode":{"allow":["a"]}}`},
	}
	for _, p := range differ {
		if a, b := normalizeSettings([]byte(p[0])), normalizeSettings([]byte(p[1])); bytes.Equal(a, b) {
			t.Errorf("%s and %s should differ", p[0], p[1])
		}
	}
}

func TestLaunchPermissionMode(t *testing.T) {
	for env, want := range map[string]string{"": "auto", "none": "", "acceptEdits": "acceptEdits", "auto": "auto"} {
		t.Setenv(PermissionModeEnv, env)
		if got := LaunchPermissionMode(); got != want {
			t.Errorf("%s=%q: mode = %q, want %q", PermissionModeEnv, env, got, want)
		}
	}
}
