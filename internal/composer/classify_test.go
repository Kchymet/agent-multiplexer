package composer_test

import (
	"strings"
	"testing"

	"amux/internal/composer"
	"amux/internal/composer/panefixture"
)

// TestPaneFixtures classifies every captured/reconstructed pane: busy panes
// (spinner, tool running, queued message) must read exactly like idle ones,
// dialogs must never read as a free composer, and only the input region's text
// counts as a draft.
func TestPaneFixtures(t *testing.T) {
	fixtures, err := panefixture.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixtures) < 10 {
		t.Fatalf("only %d pane fixtures", len(fixtures))
	}
	real := 0
	for _, f := range fixtures {
		if f.Real {
			real++
		}
		t.Run(f.Name, func(t *testing.T) {
			snap := f.Snapshot()
			got := composer.Classify(f.Runtime, snap)
			if got.State.String() != f.WantState || got.Busy != f.WantBusy || got.Text != f.WantText {
				var screen strings.Builder
				for y := 0; y < snap.Rows; y++ {
					screen.WriteString(snap.Text(y) + "\n")
				}
				t.Fatalf("Classify = {%s busy=%v text=%q}, want {%s busy=%v text=%q}; screen:\n%s",
					got.State, got.Busy, got.Text, f.WantState, f.WantBusy, f.WantText, screen.String())
			}
		})
	}
	if real == 0 {
		t.Fatal("no REAL fixtures")
	}
}

func TestMatchTyped(t *testing.T) {
	typed := "[amux] interject: PR 7 is red\nsecond line"
	for _, tc := range []struct {
		name, composer string
		want           composer.Echo
	}{
		{"nothing yet", "", composer.EchoNone},
		{"still painting", "[amux] interject: PR", composer.EchoPartial},
		{"exact", typed, composer.EchoExact},
		{"wrapped and indented", "[amux] interject: PR 7\n  is red\n  second line", composer.EchoExact},
		{"spinner glyph repainted into the region", "✻ [amux] interject: PR 7 is red\nsecond line …", composer.EchoExact},
		{"someone else's words too", typed + " and mine", composer.EchoForeign},
		{"a draft before it", "my draft " + typed, composer.EchoForeign},
		// Claude Code renders a typed backslash+Enter newline with the backslash
		// left at the line end (docs/steering.md rule 9).
		{"backslash continuations", "[amux] interject: PR 7 is red\\\nsecond line", composer.EchoExact},
		{"backslash continuation, soft-wrapped", "[amux] interject: PR 7\n  is red\\\n  second line", composer.EchoExact},
	} {
		if got := composer.MatchTyped(tc.composer, typed); got != tc.want {
			t.Errorf("%s: MatchTyped = %v, want %v", tc.name, got, tc.want)
		}
	}
}
