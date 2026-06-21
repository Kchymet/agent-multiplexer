package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/gh"
	"amux/internal/git"
	"amux/internal/store"
	"amux/internal/tmuxctl"
	"amux/internal/wsops"
)

// cmdConsole opens (or switches to) the built-in amux control console.
func cmdConsole() error { return wsops.OpenByID(context.Background(), console.ID) }

// cmdName sets the display name of the session whose window the caller is in.
func cmdName(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux name <display name>")
	}
	ctx := context.Background()
	id, err := tmuxctl.Run(ctx, "display-message", "-p", "#{@amx_ws}")
	if err != nil || strings.TrimSpace(id) == "" {
		return fmt.Errorf("not inside an amux session window")
	}
	id = strings.TrimSpace(id)
	name := strings.Join(args, " ")
	if err := wsops.Rename(id, name); err != nil {
		return err
	}
	_, _ = tmuxctl.Run(ctx, "rename-window", name)
	fmt.Printf("session %s renamed to %q\n", id, name)
	return nil
}

// ---- repo commands -------------------------------------------------------

func cmdRepo(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux repo <add|ls|rm> ...")
	}
	ctx := context.Background()
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()

	switch args[0] {
	case "add":
		switch {
		case len(args) < 2:
			repos, err := pickAndCloneRepos(ctx, db, bufio.NewReader(os.Stdin))
			if err != nil {
				return err
			}
			for _, r := range repos {
				fmt.Printf("tracked %s  <-  %s\n", r.Name, r.Source)
			}
			return nil
		case looksLikeGHRepo(args[1]):
			r, err := addRepoGH(ctx, db, args[1])
			if err != nil {
				return err
			}
			fmt.Printf("tracked %s  <-  %s\n", r.Name, r.Source)
			return nil
		default:
			r, err := addRepo(ctx, db, args[1])
			if err != nil {
				return err
			}
			fmt.Printf("tracked %s  <-  %s\n", r.Name, r.Source)
			return nil
		}
	case "ls", "list":
		repos, err := db.Repos()
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			fmt.Println("(no repositories tracked — `amux repo add <url|path>`)")
			return nil
		}
		for _, r := range repos {
			fmt.Printf("%-24s %s\n", r.Name, r.Source)
		}
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux repo rm <name>")
		}
		r, ok, err := db.Repo(args[1])
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no such repo %q", args[1])
		}
		_ = os.RemoveAll(r.GitDir)
		if err := db.DeleteRepo(args[1]); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand %q", args[0])
	}
}

func registerClone(db *store.DB, name, source string, clone func(gitDir string) error) (store.Repo, error) {
	if name == "" {
		return store.Repo{}, fmt.Errorf("could not derive a repo name from %q", source)
	}
	if existing, ok, _ := db.Repo(name); ok {
		return existing, nil
	}
	if err := os.MkdirAll(core.ReposDir(), 0o755); err != nil {
		return store.Repo{}, err
	}
	gitDir := filepath.Join(core.ReposDir(), name+".git")
	if err := clone(gitDir); err != nil {
		return store.Repo{}, err
	}
	r := store.Repo{Name: name, Source: source, GitDir: gitDir}
	return r, db.PutRepo(r)
}

func addRepo(ctx context.Context, db *store.DB, source string) (store.Repo, error) {
	return registerClone(db, git.NameFromSource(source), source, func(gitDir string) error {
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

func addRepoGH(ctx context.Context, db *store.DB, nameWithOwner string) (store.Repo, error) {
	return registerClone(db, git.NameFromSource(nameWithOwner), nameWithOwner, func(gitDir string) error {
		return gh.CloneBare(ctx, nameWithOwner, gitDir)
	})
}

func looksLikeGHRepo(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return false
	}
	if _, err := os.Stat(s); err == nil {
		return false
	}
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

const manualEntryItem = "✎  enter a URL or local path…"

func pickAndCloneRepos(ctx context.Context, db *store.DB, in *bufio.Reader) ([]store.Repo, error) {
	if !gh.Installed() {
		fmt.Println("GitHub CLI (gh) is not installed — see https://cli.github.com")
		r, err := manualEntry(ctx, db, in)
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
	owners, err := gh.Owners(ctx)
	if err != nil || len(owners) == 0 {
		return nil, fmt.Errorf("could not list GitHub owners: %v", err)
	}
	owner, err := fzfSelect("owner / org", append([]string{manualEntryItem}, owners...))
	if err != nil {
		return nil, err
	}
	if owner == manualEntryItem {
		r, err := manualEntry(ctx, db, in)
		return wrap(r, err)
	}
	remotes, err := gh.ListReposFor(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("listing %s repos: %w", owner, err)
	}
	if len(remotes) == 0 {
		return nil, fmt.Errorf("no repositories found for %s", owner)
	}
	items := []string{manualEntryItem}
	for _, r := range remotes {
		items = append(items, r.NameWithOwner)
	}
	picks, err := fzfMultiSelect("repos in "+owner, items)
	if err != nil {
		return nil, err
	}
	var result []store.Repo
	for _, p := range picks {
		if p == manualEntryItem {
			if r, err := manualEntry(ctx, db, in); err == nil {
				result = append(result, r)
			}
			continue
		}
		fmt.Printf("Cloning %s…\n", p)
		r, err := addRepoGH(ctx, db, p)
		if err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		result = append(result, r)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("nothing selected")
	}
	return result, nil
}

func wrap(r store.Repo, err error) ([]store.Repo, error) {
	if err != nil {
		return nil, err
	}
	return []store.Repo{r}, nil
}

func manualEntry(ctx context.Context, db *store.DB, in *bufio.Reader) (store.Repo, error) {
	fmt.Print("Git URL or local path: ")
	line, _ := in.ReadString('\n')
	src := strings.TrimSpace(line)
	if src == "" {
		return store.Repo{}, fmt.Errorf("no source given")
	}
	return addRepo(ctx, db, src)
}

// ---- session commands ----------------------------------------------------

func cmdSession(args []string) error {
	ctx := context.Background()
	sub := "new"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "new":
		return sessionNew(ctx)
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux session add <root-id>")
		}
		return sessionAdd(ctx, args[1])
	case "create":
		repos, cfg := parseCreateFlags(args[1:])
		var subs []wsops.SubSpec
		if len(repos) == 0 {
			subs = []wsops.SubSpec{{Agent: "claude", Mode: cfg.mode, Model: cfg.model, Prompt: cfg.prompt}}
		}
		for _, r := range repos {
			subs = append(subs, wsops.SubSpec{Repo: r, Agent: "claude", Mode: cfg.mode, Model: cfg.model, Prompt: cfg.prompt})
		}
		rootID, err := wsops.CreateRoot(ctx, cfg.name, subs)
		if err != nil {
			return err
		}
		fmt.Printf("created session %s with %d agent(s)\n", rootID, len(subs))
		return nil
	case "open":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux session open <id>")
		}
		return wsops.OpenByID(ctx, args[1])
	case "rm", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux session rm <id>")
		}
		return wsops.DeleteByID(ctx, args[1])
	case "rename":
		if len(args) < 3 {
			return fmt.Errorf("usage: amux session rename <id> <name>")
		}
		return wsops.Rename(args[1], strings.Join(args[2:], " "))
	case "ls", "list":
		return sessionList()
	default:
		return fmt.Errorf("unknown session subcommand %q", sub)
	}
}

func sessionList() error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	roots, err := db.Roots()
	if err != nil {
		return err
	}
	for _, r := range roots {
		fmt.Printf("%-8s %s\n", r.ID, r.Display())
		subs, _ := db.Children(r.ID)
		for _, s := range subs {
			info := s.Repo
			if s.Branch != "" {
				info += " · " + s.Branch
			}
			fmt.Printf("  %-8s %-8s %-6s %s\n", s.ID, defaultStr(s.Agent, "claude"), s.Mode, info)
		}
	}
	return nil
}

// sessionNew is the interactive create page (run in a tmux popup): name the
// session, drill in to add one or more agents (each a repo+branch or a plain
// agent), then Create.
func sessionNew(ctx context.Context) error {
	in := bufio.NewReader(os.Stdin)
	name := ""
	var subs []wsops.SubSpec

	for {
		menu := []string{
			fmt.Sprintf("Name       › %s", orOptional(name)),
			"+ add agent",
		}
		for i, s := range subs {
			menu = append(menu, fmt.Sprintf("  agent %d: %s", i+1, describeSub(s)))
		}
		menu = append(menu, "────────────────", "✓ Create session", "✗ Cancel")

		choice, err := fzfMenu("new session", menu)
		if err != nil {
			return nil // Esc
		}
		switch {
		case strings.HasPrefix(choice, "Name"):
			name = promptLine(in, "Session name (optional)")
		case strings.HasPrefix(choice, "+ add agent"):
			if sp, ok := configureSub(ctx, in); ok {
				subs = append(subs, sp)
			}
		case strings.HasPrefix(strings.TrimSpace(choice), "agent "):
			// selecting an agent removes it
			if i := agentIndex(choice); i >= 0 && i < len(subs) {
				subs = append(subs[:i], subs[i+1:]...)
			}
		case strings.HasPrefix(choice, "✓"):
			rootID, err := wsops.CreateRoot(ctx, name, subs)
			if err != nil {
				return err
			}
			return wsops.OpenByID(ctx, rootID)
		case strings.HasPrefix(choice, "✗"):
			return nil
		}
	}
}

func sessionAdd(ctx context.Context, rootID string) error {
	in := bufio.NewReader(os.Stdin)
	sp, ok := configureSub(ctx, in)
	if !ok {
		return nil
	}
	sub, err := wsops.AddSub(ctx, rootID, sp)
	if err != nil {
		return err
	}
	return wsops.OpenByID(ctx, sub.ID)
}

// configureSub walks the user through one agent's settings.
func configureSub(ctx context.Context, in *bufio.Reader) (wsops.SubSpec, bool) {
	repo := pickRepoForSub(ctx, in)
	sp := wsops.SubSpec{Agent: "claude", Repo: repo}
	if repo != "" {
		sp.Branch = promptLine(in, "Branch (optional, default amux/<id>)")
	}
	sp.Mode = pickMode()
	sp.Model = promptLine(in, "Model (optional, e.g. opus / sonnet)")
	sp.Prompt = promptLine(in, "Initial prompt (optional)")
	return sp, true
}

// pickRepoForSub lets the user choose a tracked repo, no repo, or clone one.
func pickRepoForSub(ctx context.Context, in *bufio.Reader) string {
	const none = "○  no repo (plain agent)"
	const clone = "➕  add / clone a repo…"
	for {
		db, err := store.Open()
		if err != nil {
			return ""
		}
		repos, _ := db.Repos()
		db.Close()
		items := []string{none, clone}
		for _, r := range repos {
			items = append(items, r.Name)
		}
		choice, err := fzfSelect("repo for this agent", items)
		if err != nil {
			return "" // cancel -> plain agent
		}
		switch choice {
		case none:
			return ""
		case clone:
			db2, _ := store.Open()
			if db2 != nil {
				_, _ = pickAndCloneRepos(ctx, db2, in)
				db2.Close()
			}
			continue
		default:
			return choice
		}
	}
}

func pickMode() string {
	choice, err := fzfMenu("mode", []string{
		"task  — short-running, tied to a temporary task",
		"loop  — long-running, (nearly) autonomous loop",
	})
	if err != nil || strings.HasPrefix(choice, "task") {
		return store.ModeTask
	}
	return store.ModeLoop
}

func describeSub(s wsops.SubSpec) string {
	repo := s.Repo
	if repo == "" {
		repo = "(plain agent)"
	}
	parts := []string{repo}
	if s.Branch != "" {
		parts = append(parts, s.Branch)
	}
	parts = append(parts, s.Mode)
	if s.Model != "" {
		parts = append(parts, s.Model)
	}
	return strings.Join(parts, " · ")
}

func agentIndex(line string) int {
	line = strings.TrimSpace(line)
	var n int
	if _, err := fmt.Sscanf(line, "agent %d:", &n); err != nil {
		return -1
	}
	return n - 1
}

// ---- shared helpers ------------------------------------------------------

type createCfg struct{ name, prompt, mode, model string }

func parseCreateFlags(args []string) ([]string, createCfg) {
	cfg := createCfg{mode: store.ModeTask}
	var repos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			cfg.name = args[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			cfg.name = strings.TrimPrefix(a, "--name=")
		case a == "--prompt" && i+1 < len(args):
			cfg.prompt = args[i+1]
			i++
		case strings.HasPrefix(a, "--prompt="):
			cfg.prompt = strings.TrimPrefix(a, "--prompt=")
		case a == "--mode" && i+1 < len(args):
			cfg.mode = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode="):
			cfg.mode = strings.TrimPrefix(a, "--mode=")
		case a == "--model" && i+1 < len(args):
			cfg.model = args[i+1]
			i++
		case strings.HasPrefix(a, "--model="):
			cfg.model = strings.TrimPrefix(a, "--model=")
		default:
			repos = append(repos, a)
		}
	}
	return repos, cfg
}

func fzfMultiSelect(prompt string, items []string) ([]string, error) {
	out, err := runFzf(items, "--multi", "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "TAB to multi-select, ENTER to confirm, ESC to cancel")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func fzfSelect(prompt string, items []string) (string, error) {
	out, err := runFzf(items, "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "type to filter, ENTER to select, ESC to cancel")
	return strings.TrimSpace(out), err
}

func fzfMenu(prompt string, items []string) (string, error) {
	out, err := runFzf(items, "--no-sort", "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "↑/↓ then ENTER, ESC to cancel")
	return strings.TrimSpace(out), err
}

func runFzf(items []string, args ...string) (string, error) {
	cmd := exec.Command("fzf", args...)
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("cancelled")
		}
		return "", fmt.Errorf("fzf not available: %w", err)
	}
	return string(out), nil
}

func promptLine(in *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func orOptional(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(optional)"
	}
	return s
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
