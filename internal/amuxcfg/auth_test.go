package amuxcfg

import (
	"os"
	"strings"
	"testing"

	"amux/internal/core"
)

func TestAuthModeDefaultsAndPersistence(t *testing.T) {
	isolate(t)
	for harness, want := range map[string]string{"claude": AuthCentral, "codex": AuthHost} {
		if got, err := AuthMode(harness); err != nil || got != want {
			t.Fatalf("%s default = %q, %v; want %q", harness, got, err, want)
		}
		if saved, err := SavedAuthMode(harness); err != nil || saved != "" {
			t.Fatalf("%s saved = %q, %v; want unset", harness, saved, err)
		}
	}
	if err := SetAuthMode("claude", AuthEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := SetCodexControl(AppServer); err != nil {
		t.Fatal(err)
	}
	if err := SetAuthMode("codex", AuthCentral); err != nil {
		t.Fatal(err)
	}
	if got, _ := AuthMode("claude"); got != AuthEnvironment {
		t.Fatalf("claude = %q", got)
	}
	if got, _ := AuthMode("codex"); got != AuthCentral {
		t.Fatalf("codex = %q", got)
	}
	// The codex section is shared with codex.control; neither clobbers the other.
	if got, _ := CodexControl(); got != AppServer {
		t.Fatalf("codex.control = %q after codex.auth write", got)
	}
	if err := SetAuthMode("codex", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := CodexControl(); got != AppServer {
		t.Fatalf("codex.control = %q after codex.auth unset", got)
	}
	if got, _ := AuthMode("codex"); got != AuthHost {
		t.Fatalf("codex after unset = %q", got)
	}
}

func TestAuthModeRejectsUnknownValues(t *testing.T) {
	isolate(t)
	if err := SetAuthMode("claude", "oauth"); err == nil {
		t.Fatal("unknown claude mode accepted")
	}
	if err := SetAuthMode("codex", AuthShared); err == nil {
		t.Fatal("legacy shared store is Claude-only")
	}
	if err := SetAuthMode("gemini", AuthCentral); err == nil {
		t.Fatal("unknown harness accepted")
	}
	if err := os.MkdirAll(core.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"claude":{"auth":"hosted"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A typo must fail closed rather than fall back to the default source.
	if _, err := AuthMode("claude"); err == nil || !strings.Contains(err.Error(), "claude.auth") {
		t.Fatalf("invalid saved mode error = %v", err)
	}
	// Repair is possible without hand-editing.
	if err := SetAuthMode("claude", AuthCentral); err != nil {
		t.Fatal(err)
	}
	if got, err := AuthMode("claude"); err != nil || got != AuthCentral {
		t.Fatalf("repaired mode = %q, %v", got, err)
	}
}
