package nativetui

import "amux/internal/core"

// Rail headers are selectable UI rows, not sessions sent by the daemon. Keep
// the complete snapshot for forms, lifecycle actions and pending attachments.
type railEntry struct {
	section string // non-empty only for a section header
	index   int    // index in sessions for an ordinary row
}

type railGroup struct{ section, id string }

var railSections = []struct{ key, hotkey, empty string }{
	{core.SectionWorkgroups, "w", "no workgroups — w to create"},
	{core.SectionRepos, "R", "no repos — R to add"},
	{core.SectionArchived, "", ""},
	{core.SectionDetached, "", ""},
}

func container(s *core.Session) bool { return s.Kind == "repo" || s.IsRoot }

func (m *model) group(s *core.Session) railGroup {
	return railGroup{section: s.Section, id: s.ID}
}

// parentIndex stays within a section: an archived agent can retain the RootID
// of a live workgroup, but collapsing that workgroup must not hide the archive.
func (m *model) parentIndex(s *core.Session) int {
	if s.RootID != "" {
		for i := range m.sessions {
			p := &m.sessions[i]
			if p.ID == s.RootID && p.Section == s.Section && container(p) {
				return i
			}
		}
	}
	return -1
}

func (m *model) hidden(s *core.Session) bool {
	if m.collapsed[railGroup{section: s.Section}] {
		return true
	}
	for n := 0; n < len(m.sessions); n++ {
		i := m.parentIndex(s)
		if i < 0 {
			break
		}
		s = &m.sessions[i]
		if m.collapsed[m.group(s)] {
			return true
		}
	}
	return false
}

func (m *model) railEntries() []railEntry {
	var entries []railEntry
	for i, s := range m.sessions {
		if s.Section == "" && !m.hidden(&s) {
			entries = append(entries, railEntry{index: i})
		}
	}
	for _, sec := range railSections {
		var children []railEntry
		populated := false
		for i, s := range m.sessions {
			if s.Section == sec.key {
				populated = true
				if !m.hidden(&s) {
					children = append(children, railEntry{index: i})
				}
			}
		}
		if populated || sec.empty != "" {
			entries = append(entries, railEntry{section: sec.key})
			entries = append(entries, children...)
		}
	}
	return entries
}

func (m *model) railSelected(e railEntry) bool {
	if e.section != "" {
		return m.sectionCursor == e.section
	}
	return m.sectionCursor == "" && m.cursor == e.index
}

func (m *model) selectEntry(e railEntry) {
	m.sectionCursor = e.section
	m.cursor = e.index
}

func (m *model) railPosition(entries []railEntry) int {
	for i, e := range entries {
		if m.railSelected(e) {
			return i
		}
	}
	return 0
}

func (m *model) railEdge(last bool) {
	entries := m.railEntries()
	if len(entries) == 0 {
		return
	}
	i := 0
	if last {
		i = len(entries) - 1
	}
	m.selectEntry(entries[i])
}

func (m *model) selectedGroup() (railGroup, bool) {
	if m.sectionCursor != "" {
		return railGroup{section: m.sectionCursor}, true
	}
	if s := m.selected(); s != nil && container(s) {
		return m.group(s), true
	}
	return railGroup{}, false
}

func (m *model) toggleRailGroup() {
	if g, ok := m.selectedGroup(); ok {
		m.setRailCollapsed(g, !m.collapsed[g])
	}
}

func (m *model) setRailCollapsed(g railGroup, collapsed bool) {
	if m.collapsed == nil {
		m.collapsed = make(map[railGroup]bool)
	}
	if collapsed {
		m.collapsed[g] = true
	} else {
		delete(m.collapsed, g)
	}
}

// Left collapses an open group; on a leaf or closed group it moves to the
// parent. Right expands a group, then moves into it on the next press.
func (m *model) railLeft() {
	if g, ok := m.selectedGroup(); ok && !m.collapsed[g] {
		m.setRailCollapsed(g, true)
		return
	}
	if s := m.selected(); s != nil {
		if i := m.parentIndex(s); i >= 0 {
			m.selectEntry(railEntry{index: i})
		} else if s.Section != "" {
			m.selectEntry(railEntry{section: s.Section})
		}
	}
}

func (m *model) railRight() {
	g, ok := m.selectedGroup()
	if !ok {
		return
	}
	if m.collapsed[g] {
		m.setRailCollapsed(g, false)
		return
	}
	entries := m.railEntries()
	i := m.railPosition(entries) + 1
	if i < len(entries) && entries[i].section == "" {
		s := &m.sessions[entries[i].index]
		if (g.id == "" && s.Section == g.section) || (s.RootID == g.id && s.Section == g.section) {
			m.selectEntry(entries[i])
		}
	}
}

// Refresh by identity, retaining folds. If a row moves under a closed group,
// select its nearest visible ancestor rather than leaving an invisible cursor.
func (m *model) refreshSessions(sessions []core.Session) {
	id := ""
	if s := m.selected(); s != nil {
		id = s.ID
	}
	m.sessions = sessions
	for i := range m.sessions {
		if id != "" && m.sessions[i].ID == id {
			m.cursor = i
			break
		}
	}
	if s := m.selected(); s != nil && m.hidden(s) {
		for n := 0; n < len(m.sessions) && m.hidden(s); n++ {
			i := m.parentIndex(s)
			if i < 0 {
				m.selectEntry(railEntry{section: s.Section})
				break
			}
			m.selectEntry(railEntry{index: i})
			s = &m.sessions[i]
		}
	}
	entries := m.railEntries()
	m.selectEntry(entries[m.railPosition(entries)])
}

// Explicit attachment reveals its ancestors, including when a newly created
// agent arrives underneath a group the user has folded.
func (m *model) reveal(s *core.Session) {
	delete(m.collapsed, railGroup{section: s.Section})
	for n := 0; n < len(m.sessions); n++ {
		i := m.parentIndex(s)
		if i < 0 {
			return
		}
		s = &m.sessions[i]
		delete(m.collapsed, m.group(s))
	}
}
