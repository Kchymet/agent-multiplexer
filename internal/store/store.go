// Package store is the durable registry of tracked repositories and workspaces,
// persisted as a single JSON file. Writes are atomic (temp file + rename) so the
// daemon (reader) never sees a half-written file.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"amux/internal/core"
)

// Repo is a tracked repository: a local bare clone used as a worktree source.
type Repo struct {
	Name   string `json:"name"`   // unique short name
	Source string `json:"source"` // original URL or path it was cloned from
	GitDir string `json:"gitDir"` // local bare clone (worktree source)
}

// Workspace modes distinguish a short, task-scoped session from a long-running,
// (nearly) autonomous loop.
const (
	ModeTask = "task" // short-running: tied to a temporary task
	ModeLoop = "loop" // long-running: a loop running nearly autonomously
)

// Workspace is an agent environment over a set of repos, each materialized as a
// git worktree under Dir. It is identified by ID; Name is an optional display
// label the agent can set later.
type Workspace struct {
	ID            string   `json:"id"`
	Name          string   `json:"name,omitempty"` // optional display name (agent-settable)
	Agent         string   `json:"agent"`          // "claude" (only one for now)
	Mode          string   `json:"mode"`           // ModeTask | ModeLoop
	Repos         []string `json:"repos"`          // repo names
	Dir           string   `json:"dir"`            // holds one worktree per repo
	InitialPrompt string   `json:"initialPrompt,omitempty"`
	// SessionID pins the agent's conversation so reopening (or restarting amux)
	// resumes the same session instead of starting fresh.
	SessionID string `json:"sessionId,omitempty"`
	Created   int64  `json:"created"`
}

// Display is the human label: the name if set, otherwise the id.
func (w Workspace) Display() string {
	if strings.TrimSpace(w.Name) != "" {
		return w.Name
	}
	return w.ID
}

// Registry is the whole persisted state.
type Registry struct {
	Repos      []Repo      `json:"repos"`
	Workspaces []Workspace `json:"workspaces"`
}

// Load reads the registry, returning an empty one if the file doesn't exist.
func Load() (*Registry, error) {
	b, err := os.ReadFile(core.RegistryPath())
	if os.IsNotExist(err) {
		return &Registry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("registry corrupt: %w", err)
	}
	return &r, nil
}

// Save writes the registry atomically.
func (r *Registry) Save() error {
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := core.RegistryPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, core.RegistryPath())
}

func (r *Registry) Repo(name string) (Repo, bool) {
	for _, x := range r.Repos {
		if x.Name == name {
			return x, true
		}
	}
	return Repo{}, false
}

func (r *Registry) WorkspaceByID(id string) (Workspace, bool) {
	for _, x := range r.Workspaces {
		if x.ID == id {
			return x, true
		}
	}
	return Workspace{}, false
}

// AddRepo inserts or replaces a repo by name.
func (r *Registry) AddRepo(repo Repo) {
	for i, x := range r.Repos {
		if x.Name == repo.Name {
			r.Repos[i] = repo
			return
		}
	}
	r.Repos = append(r.Repos, repo)
	sort.Slice(r.Repos, func(i, j int) bool { return r.Repos[i].Name < r.Repos[j].Name })
}

func (r *Registry) RemoveRepo(name string) bool {
	for i, x := range r.Repos {
		if x.Name == name {
			r.Repos = append(r.Repos[:i], r.Repos[i+1:]...)
			return true
		}
	}
	return false
}

// AddWorkspace inserts or replaces a workspace by id.
func (r *Registry) AddWorkspace(ws Workspace) {
	for i, x := range r.Workspaces {
		if x.ID == ws.ID {
			r.Workspaces[i] = ws
			return
		}
	}
	r.Workspaces = append(r.Workspaces, ws)
}

func (r *Registry) RemoveWorkspace(id string) bool {
	for i, x := range r.Workspaces {
		if x.ID == id {
			r.Workspaces = append(r.Workspaces[:i], r.Workspaces[i+1:]...)
			return true
		}
	}
	return false
}

// NewUUID returns a random RFC-4122 v4 UUID (for pinning agent session ids).
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewID returns a short unique workspace id (not colliding with existing ones).
func (r *Registry) NewID() string {
	for {
		var b [3]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if _, exists := r.WorkspaceByID(id); !exists {
			return id
		}
	}
}

// Slug normalizes an arbitrary string into a filesystem/branch-safe token.
func Slug(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// WorkspaceDir is the canonical directory for a workspace's worktrees, keyed by id.
func WorkspaceDir(id string) string {
	return filepath.Join(core.WorkspacesDir(), id)
}

// Now returns a unix-millis timestamp (kept here so callers don't import time).
func Now() int64 { return time.Now().UnixMilli() }
