// Package wsops holds session lifecycle operations shared by the daemon (rail
// actions) and the CLI. Sessions are a one-level hierarchy: a root container and
// its sub-sessions, each binding one agent to one worktree.
package wsops

import (
	"context"
	"fmt"
	"os"
	"strings"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/console"
	"amux/internal/git"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// SubSpec describes a sub-session to create under a root.
type SubSpec struct {
	Repo   string // tracked repo name; "" => no worktree, agent runs in its own dir
	Branch string // git branch (defaults to amux/<rootID> when a repo is given)
	Agent  string // defaults to "claude"
	Model  string // optional model override
	Mode   string // task | loop (defaults to task)
	Prompt string // initial prompt for this agent
}

// CreateRoot creates a root container plus its initial sub-sessions and returns
// the root id. A root always has at least one sub (a default agent if none given).
func CreateRoot(ctx context.Context, name string, subs []SubSpec) (string, error) {
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()

	rootID := db.NewID()
	dir := store.RootDir(rootID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := db.PutSession(store.Session{
		ID: rootID, RootID: "", Name: strings.TrimSpace(name),
		Mode: store.ModeTask, Dir: dir, Created: store.Now(),
	}); err != nil {
		return "", err
	}
	if len(subs) == 0 {
		subs = []SubSpec{{Agent: "claude", Mode: store.ModeTask}} // default agent
	}
	for _, sp := range subs {
		if _, err := addSub(ctx, db, rootID, sp); err != nil {
			return rootID, err
		}
	}
	return rootID, nil
}

// AddSub adds a sub-session to an existing root.
func AddSub(ctx context.Context, rootID string, sp SubSpec) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	if root, ok, _ := db.GetSession(rootID); !ok || !root.IsRoot() {
		return store.Session{}, fmt.Errorf("no such root %q", rootID)
	}
	return addSub(ctx, db, rootID, sp)
}

func addSub(ctx context.Context, db *store.DB, rootID string, sp SubSpec) (store.Session, error) {
	subID := db.NewID()
	sub := store.Session{
		ID: subID, RootID: rootID,
		Agent: defaultStr(sp.Agent, "claude"), Model: sp.Model,
		Mode: defaultStr(sp.Mode, store.ModeTask),
		Repo: sp.Repo, Prompt: sp.Prompt,
		ClaudeID: store.NewUUID(), Created: store.Now(),
	}
	if sp.Repo != "" {
		repo, ok, err := db.Repo(sp.Repo)
		if err != nil || !ok {
			return store.Session{}, fmt.Errorf("unknown repo %q", sp.Repo)
		}
		sub.Branch = defaultStr(sp.Branch, "amux/"+rootID)
		sub.Dir = store.SubDir(rootID, subID, sp.Repo, sub.Branch)
		if err := git.AddWorktree(ctx, repo.GitDir, sub.Dir, sub.Branch); err != nil {
			return store.Session{}, err
		}
	} else {
		// No repo: a plain agent in its own dir under the root.
		sub.Dir = store.SubDir(rootID, subID, "", "")
		if err := os.MkdirAll(sub.Dir, 0o755); err != nil {
			return store.Session{}, err
		}
	}
	if err := db.PutSession(sub); err != nil {
		return store.Session{}, err
	}
	return sub, nil
}

// OpenByID opens a session. The reserved console id opens the control console;
// a root opens all its sub-sessions (switching to the first); a sub opens itself.
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
		subs, err := db.Children(id)
		if err != nil {
			return err
		}
		for i, sub := range subs {
			if err := launch(ctx, sub); err != nil {
				return err
			}
			_ = i
		}
		return nil
	}
	return launch(ctx, s)
}

// launch opens (or switches to) one session's agent window.
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

	// Resume the agent's session on reopen; first launch starts fresh.
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
	if s.Repo != "" {
		return s.Repo
	}
	return s.Display()
}

// DeleteByID deletes a session. The console can't be deleted (window is closed);
// a root removes all its sub-sessions; a sub removes just itself.
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
		subs, _ := db.Children(id)
		for _, sub := range subs {
			removeSub(ctx, db, sub)
		}
		_ = os.RemoveAll(s.Dir)
		return db.DeleteSession(id)
	}
	removeSub(ctx, db, s)
	return nil
}

func removeSub(ctx context.Context, db *store.DB, sub store.Session) {
	if win := tmuxctl.WorkspaceWindows(ctx)[sub.ID]; win != "" {
		_ = tmuxctl.KillWindow(ctx, win)
	}
	if sub.Repo != "" && sub.Branch != "" {
		if repo, ok, _ := db.Repo(firstRepo(sub.Repo)); ok {
			_ = git.RemoveWorktree(ctx, repo.GitDir, sub.Dir, sub.Branch)
		}
	} else {
		_ = os.RemoveAll(sub.Dir)
	}
	_ = db.DeleteSession(sub.ID)
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

func firstRepo(repo string) string {
	if i := strings.IndexByte(repo, ','); i >= 0 {
		return repo[:i]
	}
	return repo
}
