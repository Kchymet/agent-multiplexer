package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"amux/internal/core"
	"amux/internal/gh"
	"amux/internal/git"
	"amux/internal/store"
	"amux/internal/tmuxctl"
	"amux/internal/wsops"
)

// cmdName sets the display name of the workspace whose window the caller is in.
// Intended for the agent to rename its own session: `amux name "<summary>"`.
func cmdName(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux name <display name>")
	}
	name := strings.Join(args, " ")
	ctx := context.Background()
	id, err := tmuxctl.Run(ctx, "display-message", "-p", "#{@amx_ws}")
	if err != nil || strings.TrimSpace(id) == "" {
		return fmt.Errorf("not inside an amux workspace window")
	}
	id = strings.TrimSpace(id)
	if err := wsops.Rename(id, name); err != nil {
		return err
	}
	_, _ = tmuxctl.Run(ctx, "rename-window", name) // reflect in the tmux status bar
	fmt.Printf("workspace %s renamed to %q\n", id, name)
	return nil
}

// ---- repo commands -------------------------------------------------------

func cmdRepo(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux repo <add|ls|rm> ...")
	}
	ctx := context.Background()
	switch args[0] {
	case "add":
		switch {
		case len(args) < 2:
			// No source given: fuzzy-find remote repos via gh (multi-select).
			repos, err := pickAndCloneRepos(ctx, bufio.NewReader(os.Stdin))
			if err != nil {
				return err
			}
			for _, r := range repos {
				fmt.Printf("tracked %s  <-  %s\n", r.Name, r.Source)
			}
			return nil
		case looksLikeGHRepo(args[1]):
			// OWNER/REPO shorthand: clone via gh (uses GitHub auth).
			repo, err := addRepoGH(ctx, args[1])
			if err != nil {
				return err
			}
			fmt.Printf("tracked %s  <-  %s\n", repo.Name, repo.Source)
			return nil
		default:
			repo, err := addRepo(ctx, args[1])
			if err != nil {
				return err
			}
			fmt.Printf("tracked %s  <-  %s\n", repo.Name, repo.Source)
			return nil
		}
	case "ls", "list":
		reg, err := store.Load()
		if err != nil {
			return err
		}
		if len(reg.Repos) == 0 {
			fmt.Println("(no repositories tracked — `amux repo add <url|path>`)")
			return nil
		}
		for _, r := range reg.Repos {
			fmt.Printf("%-24s %s\n", r.Name, r.Source)
		}
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux repo rm <name>")
		}
		reg, err := store.Load()
		if err != nil {
			return err
		}
		repo, ok := reg.Repo(args[1])
		if !ok {
			return fmt.Errorf("no such repo %q", args[1])
		}
		_ = os.RemoveAll(repo.GitDir)
		reg.RemoveRepo(args[1])
		if err := reg.Save(); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand %q", args[0])
	}
}

// registerClone clones into the bare repo store via clone() and registers the
// repo under name (idempotent: returns the existing repo if already tracked).
func registerClone(name, source string, clone func(gitDir string) error) (store.Repo, error) {
	if name == "" {
		return store.Repo{}, fmt.Errorf("could not derive a repo name from %q", source)
	}
	reg, err := store.Load()
	if err != nil {
		return store.Repo{}, err
	}
	if existing, ok := reg.Repo(name); ok {
		return existing, nil
	}
	if err := os.MkdirAll(core.ReposDir(), 0o755); err != nil {
		return store.Repo{}, err
	}
	gitDir := filepath.Join(core.ReposDir(), name+".git")
	if err := clone(gitDir); err != nil {
		return store.Repo{}, err
	}
	repo := store.Repo{Name: name, Source: source, GitDir: gitDir}
	reg.AddRepo(repo)
	return repo, reg.Save()
}

// addRepo clones a URL or local path into the bare repo store and registers it.
func addRepo(ctx context.Context, source string) (store.Repo, error) {
	return registerClone(git.NameFromSource(source), source, func(gitDir string) error {
		src := expandHome(source)
		if git.LooksLocal(src) {
			abs, _ := filepath.Abs(src)
			if !git.IsGitRepo(ctx, abs) {
				return fmt.Errorf("%s is not a git repository", abs)
			}
			src = abs
		}
		return git.CloneBare(ctx, src, gitDir)
	})
}

// addRepoGH clones a GitHub repo (OWNER/REPO) via gh and registers it.
func addRepoGH(ctx context.Context, nameWithOwner string) (store.Repo, error) {
	return registerClone(git.NameFromSource(nameWithOwner), nameWithOwner, func(gitDir string) error {
		return gh.CloneBare(ctx, nameWithOwner, gitDir)
	})
}

// looksLikeGHRepo reports whether s is an OWNER/REPO shorthand (not a URL or a
// local path), so `amux repo add owner/repo` clones via gh.
func looksLikeGHRepo(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return false
	}
	if _, err := os.Stat(s); err == nil {
		return false // an existing local path
	}
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

const manualEntryItem = "✎  enter a URL or local path…"

// pickAndCloneRepos fuzzy-finds remote repos via gh: it selects an owner (the
// user's account or an org), then MULTI-selects repos in that owner, and clones
// each. If gh is not authenticated it prompts to authenticate (rather than
// falling back).
func pickAndCloneRepos(ctx context.Context, in *bufio.Reader) ([]store.Repo, error) {
	if !gh.Installed() {
		fmt.Println("GitHub CLI (gh) is not installed — see https://cli.github.com")
		r, err := manualEntry(ctx, in)
		return wrap(r, err)
	}
	if !gh.Authed(ctx) {
		fmt.Print("Not signed in to GitHub. Authenticate now? [Y/n] ")
		line, _ := in.ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans == "n" || ans == "no" {
			return nil, fmt.Errorf("GitHub authentication required")
		}
		if err := gh.Login(ctx); err != nil {
			return nil, fmt.Errorf("gh auth login failed: %w", err)
		}
		if !gh.Authed(ctx) {
			return nil, fmt.Errorf("still not authenticated")
		}
	}

	// 1) pick an owner (your account + orgs you belong to).
	owners, err := gh.Owners(ctx)
	if err != nil || len(owners) == 0 {
		return nil, fmt.Errorf("could not list GitHub owners: %v", err)
	}
	owner, err := fzfSelect("owner / org", append([]string{manualEntryItem}, owners...))
	if err != nil {
		return nil, err
	}
	if owner == manualEntryItem {
		r, err := manualEntry(ctx, in)
		return wrap(r, err)
	}

	// 2) multi-select repos within that owner.
	repos, err := gh.ListReposFor(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("listing %s repos: %w", owner, err)
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("no repositories found for %s", owner)
	}
	items := []string{manualEntryItem}
	for _, r := range repos {
		items = append(items, r.NameWithOwner)
	}
	picks, err := fzfMultiSelect("repos in "+owner, items)
	if err != nil {
		return nil, err
	}

	var result []store.Repo
	manualWanted := false
	for _, p := range picks {
		if p == manualEntryItem {
			manualWanted = true
			continue
		}
		fmt.Printf("Cloning %s…\n", p)
		r, err := addRepoGH(ctx, p)
		if err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		result = append(result, r)
	}
	if manualWanted {
		if r, err := manualEntry(ctx, in); err == nil {
			result = append(result, r)
		} else {
			fmt.Printf("  %v\n", err)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("nothing selected")
	}
	return result, nil
}

// wrap turns a single (repo, err) into the slice signature.
func wrap(r store.Repo, err error) ([]store.Repo, error) {
	if err != nil {
		return nil, err
	}
	return []store.Repo{r}, nil
}

// manualEntry prompts for a git URL or local path and clones it.
func manualEntry(ctx context.Context, in *bufio.Reader) (store.Repo, error) {
	fmt.Print("Git URL or local path: ")
	line, _ := in.ReadString('\n')
	src := strings.TrimSpace(line)
	if src == "" {
		return store.Repo{}, fmt.Errorf("no source given")
	}
	return addRepo(ctx, src)
}

// ---- workspace commands --------------------------------------------------

func cmdWorkspace(args []string) error {
	ctx := context.Background()
	sub := "new"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "new":
		return workspaceNew(ctx)
	case "create":
		// Non-interactive: amux workspace create <repo>... [--name n] [--prompt t] [--mode task|loop]
		repos, cfg := parseCreateFlags(args[1:])
		if len(repos) == 0 {
			return fmt.Errorf("usage: amux workspace create <repo>... [--name n] [--prompt t] [--mode task|loop]")
		}
		cfg.Repos = repos
		ws, err := wsops.Create(ctx, cfg)
		if err != nil {
			return err
		}
		fmt.Printf("created workspace %s (%s) at %s\n", ws.ID, ws.Display(), ws.Dir)
		return nil
	case "open":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workspace open <id>")
		}
		return wsops.OpenByID(ctx, args[1])
	case "rm", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workspace rm <id>")
		}
		return wsops.Delete(ctx, args[1])
	case "rename":
		if len(args) < 3 {
			return fmt.Errorf("usage: amux workspace rename <id> <name>")
		}
		return wsops.Rename(args[1], strings.Join(args[2:], " "))
	case "ls", "list":
		reg, err := store.Load()
		if err != nil {
			return err
		}
		for _, w := range reg.Workspaces {
			fmt.Printf("%-8s %-22s %-6s %-8s %s\n", w.ID, w.Display(), w.Mode, w.Agent, strings.Join(w.Repos, ", "))
		}
		return nil
	default:
		return fmt.Errorf("unknown workspace subcommand %q", sub)
	}
}

// workspaceNew is the interactive create flow: a single configuration page (an
// fzf menu) you drill into to set repos, mode, an optional prompt, and an
// optional name, then Create. Designed to run inside a tmux popup (needs a TTY).
func workspaceNew(ctx context.Context) error {
	in := bufio.NewReader(os.Stdin)
	cfg := wsops.Config{Agent: "claude", Mode: store.ModeTask}

	for {
		menu := []string{
			fmt.Sprintf("Repos    › %s", reposSummary(cfg.Repos)),
			fmt.Sprintf("Mode     › %s", modeSummary(cfg.Mode)),
			fmt.Sprintf("Prompt   › %s", orDash(cfg.InitialPrompt)),
			fmt.Sprintf("Name     › %s", orOptional(cfg.Name)),
			"────────────────",
			"✓ Create workspace",
			"✗ Cancel",
		}
		choice, err := fzfMenu("new workspace", menu)
		if err != nil { // Esc
			return nil
		}
		switch {
		case strings.HasPrefix(choice, "Repos"):
			cfg.Repos = editRepos(ctx, in, cfg.Repos)
		case strings.HasPrefix(choice, "Mode"):
			cfg.Mode = editMode(cfg.Mode)
		case strings.HasPrefix(choice, "Prompt"):
			cfg.InitialPrompt = promptLine(in, "Initial prompt for the agent (optional)")
		case strings.HasPrefix(choice, "Name"):
			cfg.Name = promptLine(in, "Display name (optional — the agent can set this later)")
		case strings.HasPrefix(choice, "✓"):
			if len(cfg.Repos) == 0 {
				fmt.Print("Select at least one repo first. (press Enter) ")
				_, _ = in.ReadString('\n')
				continue
			}
			ws, err := wsops.Create(ctx, cfg)
			if err != nil {
				return err
			}
			fmt.Printf("Created workspace %s\n", ws.Display())
			return wsops.Open(ctx, ws)
		case strings.HasPrefix(choice, "✗"):
			return nil
		}
	}
}

// editRepos runs the repo multi-select (with the gh/clone "new repo" entry).
// Cancelling keeps the current selection.
func editRepos(ctx context.Context, in *bufio.Reader, current []string) []string {
	const cloneSentinel = "➕  add / clone a repo…"
	for {
		reg, err := store.Load()
		if err != nil {
			return current
		}
		items := []string{cloneSentinel}
		for _, r := range reg.Repos {
			items = append(items, r.Name)
		}
		picked, err := fzfMultiSelect("repos", items)
		if err != nil {
			return current // cancel keeps current
		}
		clone := false
		var sel []string
		for _, p := range picked {
			if p == cloneSentinel {
				clone = true
				continue
			}
			sel = append(sel, p)
		}
		if clone {
			if _, err := pickAndCloneRepos(ctx, in); err != nil {
				fmt.Printf("%v\n(press Enter) ", err)
				_, _ = in.ReadString('\n')
			}
			continue // reopen with the updated list
		}
		return sel
	}
}

// editMode lets the user pick the session mode.
func editMode(current string) string {
	const (
		task = "task  — short-running, tied to a temporary task"
		loop = "loop  — long-running, (nearly) autonomous loop"
	)
	choice, err := fzfMenu("mode", []string{task, loop})
	if err != nil {
		return current // keep
	}
	if strings.HasPrefix(choice, "loop") {
		return store.ModeLoop
	}
	return store.ModeTask
}

func promptLine(in *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func reposSummary(repos []string) string {
	if len(repos) == 0 {
		return "(none — required)"
	}
	return strings.Join(repos, ", ")
}

func modeSummary(mode string) string {
	if mode == store.ModeLoop {
		return "loop (long-running, autonomous)"
	}
	return "task (short-running)"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func orOptional(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(optional)"
	}
	return s
}

// parseCreateFlags splits repo names from --name/--prompt/--mode flags.
func parseCreateFlags(args []string) ([]string, wsops.Config) {
	cfg := wsops.Config{Agent: "claude", Mode: store.ModeTask}
	var repos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			cfg.Name = args[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			cfg.Name = strings.TrimPrefix(a, "--name=")
		case a == "--prompt" && i+1 < len(args):
			cfg.InitialPrompt = args[i+1]
			i++
		case strings.HasPrefix(a, "--prompt="):
			cfg.InitialPrompt = strings.TrimPrefix(a, "--prompt=")
		case a == "--mode" && i+1 < len(args):
			cfg.Mode = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode="):
			cfg.Mode = strings.TrimPrefix(a, "--mode=")
		default:
			repos = append(repos, a)
		}
	}
	return repos, cfg
}

// fzfMultiSelect pipes items into fzf --multi and returns the chosen lines.
// A non-zero/empty result (user pressed Esc) is reported as an error so callers
// abort cleanly.
func fzfMultiSelect(prompt string, items []string) ([]string, error) {
	cmd := exec.Command("fzf",
		"--multi",
		"--prompt", prompt+"> ",
		"--height", "100%",
		"--border",
		"--header", "TAB to multi-select, ENTER to confirm, ESC to cancel",
	)
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("selection cancelled")
		}
		return nil, fmt.Errorf("fzf not available: %w", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

// fzfMenu is a single-select that preserves item order (for config menus).
func fzfMenu(prompt string, items []string) (string, error) {
	cmd := exec.Command("fzf",
		"--no-sort",
		"--prompt", prompt+"> ",
		"--height", "100%",
		"--border",
		"--header", "↑/↓ then ENTER to choose, ESC to cancel",
	)
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("cancelled")
		}
		return "", fmt.Errorf("fzf not available: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// fzfSelect pipes items into fzf (single-select) and returns the chosen line.
// A cancel (Esc) is reported as an error.
func fzfSelect(prompt string, items []string) (string, error) {
	cmd := exec.Command("fzf",
		"--prompt", prompt+"> ",
		"--height", "100%",
		"--border",
		"--header", "type to filter, ENTER to select, ESC to cancel",
	)
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("selection cancelled")
		}
		return "", fmt.Errorf("fzf not available: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
