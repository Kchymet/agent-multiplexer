package core

import "time"

// AgentSessionRow preserves the agent sessions CLI's conversation metadata.
// Path is descriptive: discovery never grants filesystem or mutation access.
type AgentSessionRow struct {
	Harness  string    `json:"harness"`
	ID       string    `json:"id"`
	Cwd      string    `json:"cwd"`
	Project  string    `json:"project"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// AgentSessionsPage is an internal transport page. The CLI collects all pages
// and preserves its original text/JSON format and most-recent-first ordering.
type AgentSessionsPage struct {
	Sessions   []AgentSessionRow `json:"sessions"`
	NextCursor string            `json:"next_cursor,omitempty"`
}
