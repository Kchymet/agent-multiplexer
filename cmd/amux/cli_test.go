package main

import (
	"reflect"
	"testing"

	"amux/internal/store"
)

// TestParseCreateFlagsAgent covers the harness selector on the non-interactive
// create path: --agent picks the harness and, when no explicit --model is given,
// the model default is re-derived from that harness (so codex gets a codex model,
// not a claude one). An explicit --model always wins, and repos still fall through
// as positionals.
func TestParseCreateFlagsAgent(t *testing.T) {
	t.Run("defaults to claude", func(t *testing.T) {
		repos, cfg := parseCreateFlags([]string{"acme/api"})
		if cfg.agent != "claude" {
			t.Errorf("default agent = %q, want claude", cfg.agent)
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
		if cfg.model != store.DefaultModel("codex") {
			t.Errorf("model = %q, want the codex default %q", cfg.model, store.DefaultModel("codex"))
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

// TestCycleHarness verifies the interactive Harness toggle walks store.Harnesses
// and wraps, and that an unknown current value snaps back to the first harness.
func TestCycleHarness(t *testing.T) {
	if got := cycleHarness("claude"); got != "codex" {
		t.Errorf("cycleHarness(claude) = %q, want codex", got)
	}
	if got := cycleHarness("codex"); got != "claude" {
		t.Errorf("cycleHarness(codex) = %q, want claude (wrap)", got)
	}
	if got := cycleHarness("bogus"); got != store.Harnesses[0] {
		t.Errorf("cycleHarness(bogus) = %q, want %q", got, store.Harnesses[0])
	}
}
