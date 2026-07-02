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

// drivePicker opens a repo picker over the given repo names and applies the keys,
// returning the model so callers can assert on picker state.
func drivePicker(names []string, keys ...string) *model {
	var sessions []core.Session
	for _, n := range names {
		sessions = append(sessions, core.Session{ID: n, Kind: "repo"})
	}
	m := &model{sessions: sessions}
	m.picker = newRepoPicker("Repos", sessions, nil)
	for _, k := range keys {
		m.handlePicker(key(k))
	}
	return m
}

// The picker opens in nav mode: j/k are motions, not filter text, so vim motions
// drive the list by default and typing needs explicit intent.
func TestPickerOpensInNavMode(t *testing.T) {
	// names sort to: agent-multiplexer, harness, pipeline-engine
	names := []string{"harness", "pipeline-engine", "agent-multiplexer"}

	m := drivePicker(names, "j")
	if m.picker.searching {
		t.Fatal("picker should open in nav mode, not search")
	}
	if m.picker.filter != "" {
		t.Fatalf("nav-mode letter should not filter, got %q", m.picker.filter)
	}
	if m.picker.cursor != 1 {
		t.Fatalf("j should move cursor to 1, got %d", m.picker.cursor)
	}

	// k moves back up.
	m.handlePicker(key("k"))
	if m.picker.cursor != 0 {
		t.Fatalf("k should move cursor to 0, got %d", m.picker.cursor)
	}
}

// g/G jump to the first/last row (the last row is the track-new sentinel).
func TestPickerGotoTopBottom(t *testing.T) {
	names := []string{"a", "b", "c"}
	m := drivePicker(names, "G")
	rows := m.picker.filtered()
	if m.picker.cursor != len(rows)-1 {
		t.Fatalf("G should land on last row %d, got %d", len(rows)-1, m.picker.cursor)
	}
	m.handlePicker(key("g"))
	if m.picker.cursor != 0 {
		t.Fatalf("g should land on first row, got %d", m.picker.cursor)
	}
}

// "/" opts into search mode where letters filter; Esc returns to nav with the
// filter intact so the narrowed list can be navigated with vim motions.
func TestPickerSearchModeAndBackToNav(t *testing.T) {
	names := []string{"harness", "pipeline-engine", "agent-multiplexer"}

	m := drivePicker(names, "/", "pipe")
	if !m.picker.searching {
		t.Fatal("/ should enter search mode")
	}
	if m.picker.filter != "pipe" {
		t.Fatalf("search mode should type into filter, got %q", m.picker.filter)
	}
	if rows := m.picker.filtered(); len(rows) != 2 || rows[0].name != "pipeline-engine" {
		t.Fatalf("filter should narrow to pipeline-engine, got %+v", rows)
	}

	// Esc leaves search mode but keeps the filter; a following letter is a motion.
	m.handlePicker(key("<esc>"))
	if m.picker.searching {
		t.Fatal("esc should return to nav mode")
	}
	if m.picker.filter != "pipe" {
		t.Fatalf("esc should keep the filter, got %q", m.picker.filter)
	}
	m.handlePicker(key("j"))
	if m.picker.filter != "pipe" {
		t.Fatalf("nav-mode letter after search should not filter, got %q", m.picker.filter)
	}
}

// "i" is the other entry into search mode, mirroring the form editor's insert key.
func TestPickerInsertKeyEntersSearch(t *testing.T) {
	m := drivePicker([]string{"a", "b"}, "i")
	if !m.picker.searching {
		t.Fatal("i should enter search mode")
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
