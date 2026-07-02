package agent

import (
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

// TestHarnessModelCatalog pins each harness's model catalog and default, now
// owned by the registry rather than the store: Claude leads with opus, Codex with
// gpt-5.5, and Hermes enumerates none (amux passes no --model).
func TestHarnessModelCatalog(t *testing.T) {
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
