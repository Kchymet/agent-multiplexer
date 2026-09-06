package main

import (
	"reflect"
	"testing"

	"amux/internal/agent"
)

// TestParseCreateFlagsAgent covers the harness selector on the non-interactive
// create path: --agent picks the harness and, when no explicit --model is given,
// the model default is re-derived from that harness (so codex gets a codex model,
// not a claude one). An explicit --model always wins, and repos still fall through
// as positionals.
func TestParseCreateFlagsAgent(t *testing.T) {
	// Model discovery reads the user's live Codex config/cache. Isolate it so the
	// expected fallback does not change with whichever model the test runner has
	// selected in its own account.
	t.Setenv("CODEX_HOME", t.TempDir())

	t.Run("defaults to claude", func(t *testing.T) {
		repos, cfg := parseCreateFlags([]string{"acme/api"})
		if cfg.agent != agent.DefaultKind() {
			t.Errorf("default agent = %q, want %q", cfg.agent, agent.DefaultKind())
		}
		if !reflect.DeepEqual(repos, []string{"acme/api"}) {
			t.Errorf("repos = %v, want [acme/api]", repos)
		}
	})

	t.Run("--agent codex derives a codex model default", func(t *testing.T) {
		_, cfg := parseCreateFlags([]string{"--agent", "codex", "acme/api"})
		if cfg.agent != "codex" {
			t.Errorf("agent = %q, want codex", cfg.agent)
		}
		if want := agent.HarnessFor("codex").DefaultModel(); cfg.model != want {
			t.Errorf("model = %q, want the codex default %q", cfg.model, want)
		}
	})

	t.Run("--agent=codex form is accepted", func(t *testing.T) {
		_, cfg := parseCreateFlags([]string{"--agent=codex"})
		if cfg.agent != "codex" {
			t.Errorf("agent = %q, want codex", cfg.agent)
		}
	})

	t.Run("explicit --model overrides the derived default", func(t *testing.T) {
		_, cfg := parseCreateFlags([]string{"--agent", "codex", "--model", "gpt-5.4"})
		if cfg.model != "gpt-5.4" {
			t.Errorf("model = %q, want gpt-5.4", cfg.model)
		}
	})
}

// TestCycleHarness verifies the interactive Harness toggle walks the registered
// kinds and wraps, and that an unknown current value snaps back to the first.
func TestCycleHarness(t *testing.T) {
	kinds := agent.Kinds()
	for i, k := range kinds {
		want := kinds[(i+1)%len(kinds)]
		if got := cycleHarness(k); got != want {
			t.Errorf("cycleHarness(%q) = %q, want %q", k, got, want)
		}
	}
	if got := cycleHarness("bogus"); got != kinds[0] {
		t.Errorf("cycleHarness(bogus) = %q, want %q", got, kinds[0])
	}
}
