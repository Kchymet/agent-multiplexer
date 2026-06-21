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
			Dir: w.Dir, Created: w.Created,
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
