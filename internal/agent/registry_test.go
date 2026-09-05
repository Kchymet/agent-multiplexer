package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/store"
)

// TestRegistryLookups pins the single-switch registry surface: HarnessFor
// resolves "" to the default, a registered kind to itself, and an unknown kind to
// the no-op; Kinds/DefaultKind/Canonical/Known all read from the same registry.
func TestRegistryLookups(t *testing.T) {
	if got := DefaultKind(); got != "claude" {
		t.Fatalf("DefaultKind() = %q, want claude", got)
	}
	if got := HarnessFor("").Kind(); got != "claude" {
		t.Fatalf(`HarnessFor("").Kind() = %q, want claude`, got)
	}
	if got := Canonical(""); got != "claude" {
		t.Fatalf(`Canonical("") = %q, want claude`, got)
	}
	for _, kind := range []string{"claude", "codex", "hermes"} {
		if got := HarnessFor(kind).Kind(); got != kind {
			t.Errorf("HarnessFor(%q).Kind() = %q", kind, got)
		}
		if !Known(kind) {
			t.Errorf("Known(%q) = false, want true", kind)
		}
	}
	if !Known("") {
		t.Error(`Known("") = false, want true (resolves to default)`)
	}
	if Known("nope") {
		t.Error(`Known("nope") = true, want false`)
	}
	// An unrecognized kind degrades to the no-op harness: it still identifies as
	// that kind but supplies no launch and no durability signal.
	noop := HarnessFor("nope")
	if noop.Kind() != "nope" {
		t.Errorf("unknown kind Kind() = %q, want nope", noop.Kind())
	}
	if _, err := noop.Argv(""); err == nil {
		t.Error("noop Argv should error for an unknown kind")
	}
	// Kinds() lists the default first, in registration order.
	if got, want := Kinds(), []string{"claude", "codex", "hermes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
}

// TestHarnessDoctor: the Claude harness reports no drift against an empty (clean)
// Claude config, and harnesses with no amux-managed surface report nothing.
func TestHarnessDoctor(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // empty projects dir → no drift
	if d := HarnessFor("claude").Doctor(); len(d) != 0 {
		t.Errorf("claude Doctor on a clean config = %v, want none", d)
	}
	for _, kind := range []string{"codex", "hermes"} {
		if d := HarnessFor(kind).Doctor(); d != nil {
			t.Errorf("%s Doctor = %v, want nil", kind, d)
		}
	}
}

// TestHarnessModelCatalog pins each harness's built-in model catalog and
// default — what is offered with nothing discovered from the CLIs' config homes:
// Claude leads with opus, Codex with gpt-5.5, and Hermes enumerates none (amux
// passes no --model).
func TestHarnessModelCatalog(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	if got := HarnessFor("claude").Models(); len(got) == 0 || got[0] != "opus" {
		t.Errorf("claude Models() = %v, want opus first", got)
	}
	if got := HarnessFor("claude").DefaultModel(); got != "opus" {
		t.Errorf("claude DefaultModel() = %q, want opus", got)
	}
	if got := HarnessFor("codex").DefaultModel(); got != "gpt-5.5" {
		t.Errorf("codex DefaultModel() = %q, want gpt-5.5", got)
	}
	if got := HarnessFor("hermes").Models(); len(got) != 0 {
		t.Errorf("hermes Models() = %v, want none", got)
	}
	if got := HarnessFor("hermes").DefaultModel(); got != "" {
		t.Errorf("hermes DefaultModel() = %q, want empty", got)
	}
}

// TestNewSessionID pins the pre-mint policy: Claude accepts a pinned id (non-empty
// uuid), while Codex and Hermes mint their own on first run (empty).
func TestNewSessionID(t *testing.T) {
	if got := HarnessFor("claude").NewSessionID(); got == "" {
		t.Error("claude NewSessionID() should pre-mint a uuid")
	}
	for _, kind := range []string{"codex", "hermes"} {
		if got := HarnessFor(kind).NewSessionID(); got != "" {
			t.Errorf("%s NewSessionID() = %q, want empty (self-minted)", kind, got)
		}
	}
}

// TestPlanLaunchFresh covers the fresh-launch path shared by every harness: with
// no pinned conversation and no existing session on disk, the launch dir is
// unchanged and the prompt is passed as the sole trailing arg.
func TestPlanLaunchFresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	for _, kind := range []string{"claude", "codex", "hermes"} {
		req := LaunchRequest{Session: store.Session{ID: "a1", Agent: kind}, Dir: dir, Prompt: "do it"}
		got := HarnessFor(kind).PlanLaunch(req)
		if got.Dir != dir {
			t.Errorf("%s PlanLaunch dir = %q, want %q", kind, got.Dir, dir)
		}
		if !reflect.DeepEqual(got.Extra, []string{"do it"}) {
			t.Errorf("%s PlanLaunch extra = %v, want [do it]", kind, got.Extra)
		}
	}
}

// TestArgvThroughHarness verifies the launch argv is built by the harness (the
// free-func agent.Argv delegates to it): Hermes launches `hermes chat` with -m.
func TestArgvThroughHarness(t *testing.T) {
	t.Setenv("PATH", "/nonexistent")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_HERMES_BIN", "hermes-amux-test")
	argv, err := HarnessFor("hermes").Argv("m1")
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	if want := []string{"hermes-amux-test", "chat", "-m", "m1"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("hermes Argv = %v, want %v", argv, want)
	}
}

// TestHarnessModelsDiscovered pins that the catalogs are live, not baked in:
// Codex offers exactly what its models cache lists (in its order, so its top
// pick becomes the default) plus the user's configured model even when the
// cache doesn't list it; Claude keeps its aliases first and appends the extra
// models Claude Code cached for the account plus the user's configured model.
func TestHarnessModelsDiscovered(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cache := `{"models":[
		{"slug":"gpt-5.5-astra","visibility":"list","priority":1},
		{"slug":"gpt-5.5","visibility":"list","priority":2},
		{"slug":"gpt-5.5-hidden","visibility":"hide","priority":0}]}`
	if err := os.WriteFile(filepath.Join(codexHome, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"gpt-5.5-hidden\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"gpt-5.5-astra", "gpt-5.5", "gpt-5.5-hidden"}
	if got := HarnessFor("codex").Models(); !reflect.DeepEqual(got, want) {
		t.Errorf("codex Models() = %v, want %v", got, want)
	}
	if got := HarnessFor("codex").DefaultModel(); got != "gpt-5.5-astra" {
		t.Errorf("codex DefaultModel() = %q, want the cache's top pick", got)
	}

	claudeHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	cfg := `{"model":"claude-opus-5","additionalModelOptionsCache":[
		{"label":"Fable","value":"claude-fable-5-1[1m]"},
		{"label":"Opus","value":"opus"}]}`
	if err := os.WriteFile(filepath.Join(claudeHome, ".claude.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	want = []string{"opus", "sonnet", "haiku", "fable", "claude-fable-5-1[1m]", "claude-opus-5"}
	if got := HarnessFor("claude").Models(); !reflect.DeepEqual(got, want) {
		t.Errorf("claude Models() = %v, want %v", got, want)
	}
	if got := HarnessFor("claude").DefaultModel(); got != "opus" {
		t.Errorf("claude DefaultModel() = %q, want opus regardless of extras", got)
	}
}
