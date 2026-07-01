package nativetui

import (
	"strings"
	"testing"

	"amux/internal/core"
)

func TestFuzzyMatch(t *testing.T) {
	tests := []struct {
		query, cand string
		want        bool
	}{
		{"", "anything", true}, // empty query matches all
		{"api", "api", true},   // exact
		{"api", "API", true},   // case-insensitive
		{"ac", "agent-multiplexer", false},
		{"amx", "agent-multiplexer", true}, // subsequence
		{"pipe", "pipeline-engine", true},
		{"gine", "pipeline-engine", true},
		{"xyz", "pipeline-engine", false},
		{"harness", "harness", true},
		{"hrs", "harness", true},
	}
	for _, tt := range tests {
		if got := fuzzyMatch(tt.query, tt.cand); got != tt.want {
			t.Errorf("fuzzyMatch(%q, %q) = %v, want %v", tt.query, tt.cand, got, tt.want)
		}
	}
}

func TestNewRepoPickerSeedsAndLists(t *testing.T) {
	sessions := []core.Session{
		{ID: "harness", Kind: "repo", Title: "harness"},
		{ID: "agent-multiplexer", Kind: "repo", Title: "agent-multiplexer"},
		{ID: "some-agent", Kind: "claude"}, // not a repo — must be ignored
	}
	p := newRepoPicker("Repos", sessions, []string{"harness"})
	if !p.selected["harness"] {
		t.Error("seed repo should start selected")
	}
	if len(p.items) != 2 {
		t.Fatalf("want 2 repo items, got %d", len(p.items))
	}
	// Items are sorted; the non-repo row must not appear.
	if p.items[0].name != "agent-multiplexer" || p.items[1].name != "harness" {
		t.Errorf("items not sorted repo-only: %+v", p.items)
	}
}

func TestPickerFilteredHasTrackNewAndChosenOrder(t *testing.T) {
	sessions := []core.Session{
		{ID: "harness", Kind: "repo"},
		{ID: "pipeline-engine", Kind: "repo"},
		{ID: "agent-multiplexer", Kind: "repo"},
	}
	p := newRepoPicker("Repos", sessions, []string{"pipeline-engine", "harness"})

	// The track-new row is always the last filtered entry.
	rows := p.filtered()
	if rows[len(rows)-1].name != trackNewID {
		t.Error("track-new row should be last in the filtered list")
	}

	// Filtering narrows to matches (plus the track-new row).
	p.filter = "pipe"
	rows = p.filtered()
	if len(rows) != 2 || rows[0].name != "pipeline-engine" {
		t.Errorf("filter %q gave %+v", p.filter, rows)
	}

	// chosen() returns selected names in sorted items order, not selection order.
	if got := strings.Join(p.chosen(), ","); got != "harness,pipeline-engine" {
		t.Errorf("chosen() = %q, want %q", got, "harness,pipeline-engine")
	}
}
