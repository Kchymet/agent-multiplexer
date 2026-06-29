package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"amux/internal/core"
)

// Now returns a unix-millis timestamp.
func Now() int64 { return time.Now().UnixMilli() }

// RootDir is the container directory for a root session's sub-worktrees.
func RootDir(rootID string) string { return filepath.Join(core.SessionsDir(), rootID) }

// SubDir is the worktree directory for a sub-session under its root.
func SubDir(rootID, subID, repo, branch string) string {
	label := strings.Trim(Slug(repo)+"-"+Slug(branch), "-")
	if label == "" {
		label = subID
	}
	return filepath.Join(RootDir(rootID), label)
}

// AgentDir is an agent's base directory; it holds one worktree subdir per repo
// the agent works on, so the agent operates across its own worktrees only.
func AgentDir(rootID, agentID string) string {
	return filepath.Join(core.SessionsDir(), rootID, agentID)
}

// SplitRepos parses a comma-separated repo list (trimming blanks).
func SplitRepos(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// JoinRepos serializes a repo list.
func JoinRepos(repos []string) string { return strings.Join(repos, ",") }

// BackfillWorkspaceRepos gives each root (workspace) an attached-repo list — the
// union of its agents' repos — for roots that don't have one yet. Idempotent.
func (d *DB) BackfillWorkspaceRepos() error {
	roots, err := d.Roots()
	if err != nil {
		return err
	}
	for _, r := range roots {
		if strings.TrimSpace(r.Repo) != "" {
			continue
		}
		subs, _ := d.Children(r.ID)
		seen := map[string]bool{}
		var union []string
		for _, s := range subs {
			for _, rp := range SplitRepos(s.Repo) {
				if !seen[rp] {
					seen[rp] = true
					union = append(union, rp)
				}
			}
		}
		if len(union) > 0 {
			r.Repo = JoinRepos(union)
			_ = d.PutSession(r)
		}
	}
	return nil
}

// legacy mirrors the old JSON registry shape (pre-SQLite).
type legacy struct {
	Repos []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
		GitDir string `json:"gitDir"`
	} `json:"repos"`
	Workspaces []struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Agent         string   `json:"agent"`
		Mode          string   `json:"mode"`
		Repos         []string `json:"repos"`
		Dir           string   `json:"dir"`
		InitialPrompt string   `json:"initialPrompt"`
		SessionID     string   `json:"sessionId"`
		Created       int64    `json:"created"`
	} `json:"workspaces"`
}

// importLegacy one-time imports the old JSON registry into the DB, then renames
// the JSON aside as a backup. It is a no-op if the DB already has sessions or no
// registry file exists. Each old (multi-repo, single-agent) workspace becomes a
// root container plus one sub-session that preserves its dir + claude session.
func (d *DB) importLegacy() error {
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil // already have sessions; don't re-import
	}
	regPath := core.RegistryPath()
	b, err := os.ReadFile(regPath)
	if err != nil {
		return nil // no legacy registry
	}
	var lg legacy
	if err := json.Unmarshal(b, &lg); err != nil {
		return err
	}

	for _, r := range lg.Repos {
		_ = d.PutRepo(Repo{Name: r.Name, Source: r.Source, GitDir: r.GitDir})
	}
	for _, w := range lg.Workspaces {
		mode := w.Mode
		if mode == "" {
			mode = ModeTask
		}
		// root container
		_ = d.PutSession(Session{
			ID: w.ID, RootID: "", Name: w.Name, Mode: mode,
			Dir: w.Dir, Created: w.Created, Scope: ScopeWork,
		})
		// one sub preserving the legacy combined dir + claude session
		_ = d.PutSession(Session{
			ID: d.NewID(), RootID: w.ID, Agent: defaultStr(w.Agent, "claude"), Mode: mode,
			Repo: strings.Join(w.Repos, ","), Dir: w.Dir,
			ClaudeID: w.SessionID, Prompt: w.InitialPrompt, Created: w.Created,
		})
	}

	_ = os.Rename(regPath, regPath+".migrated") // keep as backup, don't delete
	return nil
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// Session modes.
const (
	ModeTask = "task"
	ModeLoop = "loop"
)

// Workgroup scopes (root sessions only).
const (
	ScopeWork = "work" // cross-repo workgroup: root + N agents
	ScopeRepo = "repo" // single-repo, single-member workgroup nested under its repo
)
