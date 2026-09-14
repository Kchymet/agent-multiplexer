package agent

import "amux/internal/core"

// ListSessionRows discovers conversations using the daemon's configured homes.
// It intentionally includes other sessions and untracked user conversations.
func ListSessionRows() []core.AgentSessionRow {
	var rows []core.AgentSessionRow
	for _, h := range Harnesses() {
		for _, s := range h.ListSessions() {
			rows = append(rows, core.AgentSessionRow{
				Harness: h.Kind(), ID: s.ID, Cwd: s.Cwd, Project: s.Project,
				Path: s.Path, Size: s.Size, Modified: s.Modified,
			})
		}
	}
	return rows
}
