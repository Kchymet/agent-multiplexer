package nativetui

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
)

// The dashboard remembers its rail view across restarts: which groups are
// folded and which row is selected. The daemon's snapshot stays authoritative —
// saved state only decorates ids the snapshot still carries, so an archived or
// deleted session never comes back, and stale ids drop out at the next save.

const (
	railStateVersion  = 1
	railStateDebounce = 500 * time.Millisecond
)

// railStateFile is the on-disk form. Groups lists every group that was on the
// rail at save time with its fold, so a restart reproduces what the user left;
// a group missing from it is new and gets the live default (expanded).
type railStateFile struct {
	Version  int              `json:"version"`
	Groups   []railStateGroup `json:"groups,omitempty"`
	Selected railStateSel     `json:"selected,omitempty"`
}

type railStateGroup struct {
	Section   string `json:"section"`
	ID        string `json:"id,omitempty"` // empty for a section header
	Collapsed bool   `json:"collapsed"`
}

// railStateSel is the selected row: a session id, or a section header.
type railStateSel struct {
	ID      string `json:"id,omitempty"`
	Section string `json:"section,omitempty"`
}

// railStateStore persists one dashboard's view state. A nil store (tests, or a
// dashboard without a state dir) disables persistence.
type railStateStore struct {
	path   string
	saved  []byte // last bytes written (or loaded), to skip redundant writes
	serial uint64 // debounce generation: only the newest tick writes
	// restore is the selection to apply on the first snapshot, then cleared.
	restore *railStateSel
	// ready is set once a snapshot has been reconciled: saving before then
	// would prune every group against an empty rail.
	ready bool
}

func railStatePath() string { return filepath.Join(core.StateDir(), "tui-state.json") }

// loadRailState reads the saved view. A missing, unreadable, corrupt or
// future-version file yields an empty state — the dashboard falls back to its
// defaults and never fails to open over it.
func loadRailState(path string) (railStateFile, []byte) {
	b, err := os.ReadFile(path)
	if err != nil {
		return railStateFile{}, nil
	}
	var st railStateFile
	if json.Unmarshal(b, &st) != nil || st.Version != railStateVersion {
		return railStateFile{}, nil
	}
	return st, b
}

// openRailState loads the saved view into m: folds apply immediately (they are
// keyed by id and only matter once the id is on the rail); the selection waits
// for the first snapshot.
func (m *model) openRailState(path string) {
	st, raw := loadRailState(path)
	m.state = &railStateStore{path: path, saved: raw}
	for _, g := range st.Groups {
		if g.Collapsed && g.Section != "" {
			m.setRailCollapsed(railGroup{section: g.Section, id: g.ID}, true)
		}
	}
	if st.Selected.ID != "" || st.Selected.Section != "" {
		sel := st.Selected
		m.state.restore = &sel
	}
}

// applyRailRestore reconciles the saved selection with the first snapshot. An
// id that is gone or archived is ignored; one now hidden under a fold selects
// its nearest visible ancestor (refreshSessions) rather than unfolding it.
func (m *model) applyRailRestore() {
	if m.state == nil || m.state.ready {
		return
	}
	m.state.ready = true
	// Drop folds for ids that did not survive the restart, so one that comes
	// back later (restored from the archive) starts from the default.
	live := map[railGroup]bool{}
	for _, g := range m.railStateSnapshot().Groups {
		live[railGroup{section: g.Section, id: g.ID}] = true
	}
	for g := range m.collapsed {
		if !live[g] {
			delete(m.collapsed, g)
		}
	}
	sel := m.state.restore
	m.state.restore = nil
	if sel == nil {
		return
	}
	switch {
	case sel.ID != "":
		for i := range m.sessions {
			if s := m.sessions[i]; s.ID == sel.ID && !archivedSession(s) {
				m.cursor, m.sectionCursor = i, ""
				m.refreshSessions(m.sessions)
				return
			}
		}
	case sel.Section != "":
		for _, e := range m.railEntries() {
			if e.section == sel.Section {
				m.selectEntry(e)
				return
			}
		}
	}
}

// railStateSnapshot is the view to persist, reconciled against the current
// snapshot: only live sections and live (unarchived) containers are recorded,
// which prunes ids that were archived or deleted.
func (m *model) railStateSnapshot() railStateFile {
	st := railStateFile{Version: railStateVersion}
	for _, e := range m.railEntries() {
		if e.section != "" {
			g := railGroup{section: e.section}
			st.Groups = append(st.Groups, railStateGroup{Section: g.section, Collapsed: m.collapsed[g]})
		}
	}
	for i := range m.sessions {
		s := &m.sessions[i]
		if container(s) && s.Section != "" && !archivedSession(*s) {
			g := m.group(s)
			st.Groups = append(st.Groups, railStateGroup{Section: g.section, ID: g.id, Collapsed: m.collapsed[g]})
		}
	}
	if m.sectionCursor != "" {
		st.Selected.Section = m.sectionCursor
	} else if s := m.selected(); s != nil && !archivedSession(*s) {
		st.Selected.ID = s.ID
	}
	return st
}

type railStateTickMsg struct{ serial uint64 }

// persistRailState schedules a debounced save when the view changed since the
// last write. Bursts (holding an arrow key) coalesce into one write.
func (m *model) persistRailState() tea.Cmd {
	if m.state == nil || !m.state.ready {
		return nil
	}
	b, err := json.Marshal(m.railStateSnapshot())
	if err != nil || string(b) == string(m.state.saved) {
		return nil
	}
	m.state.serial++
	serial := m.state.serial
	return tea.Tick(railStateDebounce, func(time.Time) tea.Msg { return railStateTickMsg{serial} })
}

// flushRailState writes the current view now if it differs from what is on
// disk. Errors are swallowed: losing a fold is not worth interrupting the user.
func (m *model) flushRailState() {
	if m.state == nil || !m.state.ready {
		return
	}
	b, err := json.Marshal(m.railStateSnapshot())
	if err != nil || string(b) == string(m.state.saved) {
		return
	}
	if writeRailState(m.state.path, b) == nil {
		m.state.saved = b
	}
}

func writeRailState(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tui-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
