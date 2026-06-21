// Package wsops holds workspace lifecycle operations shared by the daemon (rail
// actions) and the CLI (popup flows): creating, opening/switching, renaming, and
// deleting. Workspaces are identified by id; names are optional display labels.
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

// Open switches to the workspace's window if it's running, otherwise starts the
// agent in the workspace dir, tags the window with the workspace id, and
// switches to it.
func Open(ctx context.Context, ws store.Workspace) error {
	if win := tmuxctl.WorkspaceWindows(ctx)[ws.ID]; win != "" {
		_ = tmuxctl.SwitchClient(ctx, win) // best-effort (needs a client)
		return nil
	}
	if _, err := os.Stat(ws.Dir); err != nil {
		return fmt.Errorf("workspace dir missing: %s", ws.Dir)
	}
	// Pre-trust the dir so Claude Code skips its interactive "trust this folder?"
	// dialog (amux created this dir; best-effort).
	if ws.Agent == "" || ws.Agent == "claude" {
		_ = claudecfg.TrustDir(ws.Dir)
	}
	var extra []string
	if p := strings.TrimSpace(ws.InitialPrompt); p != "" {
		extra = append(extra, p)
	}
	argv, err := agent.Argv(ws.Agent, extra...)
	if err != nil {
		return err
	}
	mode := ws.Mode
	if mode == "" {
		mode = store.ModeTask
	}
	// amux is a UI layer: it signals the session's intent to the agent and lets
	// the agent (or a cloud orchestrator) own autonomy. The agent's launch
	// command (overridable via AMUX_CLAUDE_BIN) can branch on AMUX_MODE.
	env := []string{
		"AMUX_WORKSPACE=" + ws.ID,
		"AMUX_MODE=" + mode,
		"AMUX_AGENT=" + ws.Agent,
	}
	win, err := tmuxctl.NewWindow(ctx, ws.Dir, ws.Display(), env, argv...)
	if err != nil {
		return err
	}
	_ = tmuxctl.TagWorkspace(ctx, win, ws.ID)
	_ = tmuxctl.SwitchClient(ctx, win)
	return nil
}

// OpenByID loads the workspace and opens it. The reserved console id opens the
// built-in amux control console instead of a registry workspace.
func OpenByID(ctx context.Context, id string) error {
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return err
		}
		return Open(ctx, console.Workspace())
	}
	reg, err := store.Load()
	if err != nil {
		return err
	}
	ws, ok := reg.WorkspaceByID(id)
	if !ok {
		return fmt.Errorf("no such workspace %q", id)
	}
	return Open(ctx, ws)
}

// Delete closes the workspace window, removes every repo worktree and its
// branch, deletes the workspace dir, and drops it from the registry.
func Delete(ctx context.Context, id string) error {
	if id == console.ID {
		// The console can't be deleted; just close its window if open.
		if win := tmuxctl.WorkspaceWindows(ctx)[id]; win != "" {
			_ = tmuxctl.KillWindow(ctx, win)
		}
		return nil
	}
	reg, err := store.Load()
	if err != nil {
		return err
	}
	ws, ok := reg.WorkspaceByID(id)
	if !ok {
		return fmt.Errorf("no such workspace %q", id)
	}
	if win := tmuxctl.WorkspaceWindows(ctx)[id]; win != "" {
		_ = tmuxctl.KillWindow(ctx, win)
	}
	branch := "amux/" + ws.ID
	for _, repoName := range ws.Repos {
		if repo, ok := reg.Repo(repoName); ok {
			_ = git.RemoveWorktree(ctx, repo.GitDir, filepath.Join(ws.Dir, repoName), branch)
		}
	}
	_ = os.RemoveAll(ws.Dir)
	reg.RemoveWorkspace(id)
	return reg.Save()
}

// Rename sets a workspace's display name.
func Rename(id, name string) error {
	reg, err := store.Load()
	if err != nil {
		return err
	}
	ws, ok := reg.WorkspaceByID(id)
	if !ok {
		return fmt.Errorf("no such workspace %q", id)
	}
	ws.Name = strings.TrimSpace(name)
	reg.AddWorkspace(ws)
	return reg.Save()
}

// Config is the user-chosen settings for a new workspace.
type Config struct {
	Name          string // optional display name
	Agent         string // "claude"
	Mode          string // store.ModeTask | store.ModeLoop
	Repos         []string
	InitialPrompt string
}

// Create materializes worktrees for the selected repos and registers a new
// workspace with a generated id. It does not open it (callers decide when).
func Create(ctx context.Context, cfg Config) (store.Workspace, error) {
	reg, err := store.Load()
	if err != nil {
		return store.Workspace{}, err
	}
	if cfg.Agent == "" {
		cfg.Agent = "claude"
	}
	if cfg.Mode == "" {
		cfg.Mode = store.ModeTask
	}
	id := reg.NewID()
	dir := store.WorkspaceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Workspace{}, err
	}
	branch := "amux/" + id
	for _, rn := range cfg.Repos {
		repo, ok := reg.Repo(rn)
		if !ok {
			return store.Workspace{}, fmt.Errorf("unknown repo %q", rn)
		}
		if err := git.AddWorktree(ctx, repo.GitDir, filepath.Join(dir, rn), branch); err != nil {
			return store.Workspace{}, err
		}
	}
	ws := store.Workspace{
		ID:            id,
		Name:          strings.TrimSpace(cfg.Name),
		Agent:         cfg.Agent,
		Mode:          cfg.Mode,
		Repos:         cfg.Repos,
		Dir:           dir,
		InitialPrompt: cfg.InitialPrompt,
		Created:       store.Now(),
	}
	reg.AddWorkspace(ws)
	if err := reg.Save(); err != nil {
		return store.Workspace{}, err
	}
	return ws, nil
}
