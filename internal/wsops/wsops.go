// Package wsops holds session lifecycle operations shared by the daemon (rail
// actions) and the CLI. A workspace (root) is a template that attaches repos but
// checks out nothing itself; its agents (subs) each work on a subset of those
// repos, one worktree per repo under the agent's own directory.
package wsops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/console"
	"amux/internal/git"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// AgentSpec describes an agent to create under a workspace.
type AgentSpec struct {
	Repos  []string // subset of the workspace's repos this agent works on
	Agent  string   // defaults to "claude"
	Model  string   // optional model override
	Mode   string   // task | loop (defaults to task)
	Prompt string   // initial prompt
}

// CreateWorkspace creates a workspace (root) that attaches repos but checks out
// nothing itself, optionally with a default agent that uses all of them. Returns
// the workspace id.
func CreateWorkspace(ctx context.Context, name string, repos []string, withDefaultAgent bool) (string, error) {
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()

	rootID := db.NewID()
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Name: strings.TrimSpace(name),
		Mode: store.ModeTask, Repo: store.JoinRepos(repos), Created: store.Now(),
	}); err != nil {
		return "", err
	}
	if withDefaultAgent {
		if _, err := addAgent(ctx, db, rootID, AgentSpec{Repos: repos, Agent: "claude", Mode: store.ModeTask}); err != nil {
			return rootID, err
		}
	}
	return rootID, nil
}

// AddAgent adds an agent (sub-session) to a workspace.
func AddAgent(ctx context.Context, rootID string, spec AgentSpec) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok || !root.IsRoot() {
		return store.Session{}, fmt.Errorf("no such workspace %q", rootID)
	}
	return addAgent(ctx, db, rootID, spec)
}

func addAgent(ctx context.Context, db *store.DB, rootID string, spec AgentSpec) (store.Session, error) {
	agentID := db.NewID()
	dir := store.AgentDir(rootID, agentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Session{}, err
	}
	branch := "amux/" + rootID + "/" + agentID
	for _, repoName := range spec.Repos {
		repo, ok, err := db.Repo(repoName)
		if err != nil || !ok {
			return store.Session{}, fmt.Errorf("unknown repo %q", repoName)
		}
		if err := git.AddWorktree(ctx, repo.GitDir, filepath.Join(dir, repoName), branch); err != nil {
			return store.Session{}, err
		}
	}
	a := store.Session{
		ID: agentID, RootID: rootID,
		Agent: defaultStr(spec.Agent, "claude"), Model: spec.Model,
		Mode: defaultStr(spec.Mode, store.ModeTask),
		Repo: store.JoinRepos(spec.Repos), Branch: branch, Dir: dir,
		ClaudeID: store.NewUUID(), Prompt: spec.Prompt, Created: store.Now(),
	}
	if err := db.PutSession(a); err != nil {
		return store.Session{}, err
	}
	return a, nil
}

// AttachRepo adds a repo to a workspace's attached set (template only; existing
// agents are unchanged — assign it to an agent by creating/adding one).
func AttachRepo(rootID, repo string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok || !root.IsRoot() {
		return fmt.Errorf("no such workspace %q", rootID)
	}
	if _, ok, _ := db.Repo(repo); !ok {
		return fmt.Errorf("unknown repo %q", repo)
	}
	repos := store.SplitRepos(root.Repo)
	for _, r := range repos {
		if r == repo {
			return nil // already attached
		}
	}
	root.Repo = store.JoinRepos(append(repos, repo))
	return db.PutSession(root)
}

// OpenByID opens a session: the console, a workspace (opens all its agents), or
// an agent (its window).
func OpenByID(ctx context.Context, id string) error {
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return err
		}
		return launch(ctx, console.Session())
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if s.IsRoot() {
		agents, err := db.Children(id)
		if err != nil {
			return err
		}
		if len(agents) == 0 {
			return fmt.Errorf("workspace %q has no agents yet (add one with `a`)", id)
		}
		for _, a := range agents {
			if err := launch(ctx, a); err != nil {
				return err
			}
		}
		return nil
	}
	return launch(ctx, s)
}

// launch opens (or switches to) one agent's window.
func launch(ctx context.Context, s store.Session) error {
	if win := tmuxctl.WorkspaceWindows(ctx)[s.ID]; win != "" {
		_ = tmuxctl.SwitchClient(ctx, win)
		return nil
	}
	if _, err := os.Stat(s.Dir); err != nil {
		return fmt.Errorf("session dir missing: %s", s.Dir)
	}
	if s.Agent == "" || s.Agent == "claude" {
		_ = claudecfg.TrustDir(s.Dir)
	}

	prompt := strings.TrimSpace(s.Prompt)
	var extra []string
	switch {
	case s.ClaudeID != "" && claudecfg.SessionExists(s.Dir, s.ClaudeID):
		extra = []string{"--resume", s.ClaudeID}
	case s.ClaudeID != "":
		extra = []string{"--session-id", s.ClaudeID}
		if prompt != "" {
			extra = append(extra, prompt)
		}
	case claudecfg.AnySession(s.Dir):
		extra = []string{"--continue"}
	default:
		if prompt != "" {
			extra = []string{prompt}
		}
	}
	argv, err := agent.Argv(s.Agent, s.Model, extra...)
	if err != nil {
		return err
	}
	env := []string{
		"AMUX_WORKSPACE=" + s.ID,
		"AMUX_ROOT=" + s.RootID,
		"AMUX_MODE=" + defaultStr(s.Mode, store.ModeTask),
		"AMUX_AGENT=" + defaultStr(s.Agent, "claude"),
	}
	win, err := tmuxctl.NewWindow(ctx, s.Dir, windowName(s), env, argv...)
	if err != nil {
		return err
	}
	_ = tmuxctl.TagWorkspace(ctx, win, s.ID)
	_ = tmuxctl.SwitchClient(ctx, win)
	return nil
}

func windowName(s store.Session) string {
	if repos := store.SplitRepos(s.Repo); len(repos) > 0 {
		return strings.Join(repos, "+")
	}
	return s.Display()
}

// DeleteByID deletes a session. The console can't be deleted (window closed); a
// workspace removes all its agents; an agent removes just itself.
func DeleteByID(ctx context.Context, id string) error {
	if id == console.ID {
		if win := tmuxctl.WorkspaceWindows(ctx)[id]; win != "" {
			_ = tmuxctl.KillWindow(ctx, win)
		}
		return nil
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		return fmt.Errorf("no such session %q", id)
	}
	if s.IsRoot() {
		agents, _ := db.Children(id)
		for _, a := range agents {
			removeAgent(ctx, db, a)
		}
		_ = os.RemoveAll(store.RootDir(id))
		return db.DeleteSession(id)
	}
	removeAgent(ctx, db, s)
	return nil
}

func removeAgent(ctx context.Context, db *store.DB, a store.Session) {
	if win := tmuxctl.WorkspaceWindows(ctx)[a.ID]; win != "" {
		_ = tmuxctl.KillWindow(ctx, win)
	}
	for _, repoName := range store.SplitRepos(a.Repo) {
		if repo, ok, _ := db.Repo(repoName); ok {
			_ = git.RemoveWorktree(ctx, repo.GitDir, filepath.Join(a.Dir, repoName), a.Branch)
		}
	}
	_ = os.RemoveAll(a.Dir)
	_ = db.DeleteSession(a.ID)
}

// Rename sets a session's display name.
func Rename(id, name string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		return fmt.Errorf("no such session %q", id)
	}
	s.Name = strings.TrimSpace(name)
	return db.PutSession(s)
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
