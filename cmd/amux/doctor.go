package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/gh"
	"amux/internal/store"
	"amux/internal/tmuxctl"
)

// cmdDoctor prints a health summary: required/optional CLI dependencies and the
// amux runtime (daemon, database, isolated tmux server). Exits non-zero if a
// required dependency is missing.
func cmdDoctor() error {
	ctx := context.Background()
	fmt.Println("amux doctor\n")

	deps := []struct {
		bin, verArg, note string
		required          bool
	}{
		{"tmux", "-V", "isolated tmux server (needs 3.x)", true},
		{"git", "--version", "bare clones & worktrees", true},
		{"claude", "--version", "default agent", true},
		{"fzf", "--version", "interactive pickers (new workspace/agent)", false},
		{"gh", "--version", "browse & clone GitHub repos", false},
		{"hermes", "--version", "alternate agent", false},
	}

	missingRequired := false
	fmt.Println("Dependencies")
	for _, d := range deps {
		path, via := resolveCmd(ctx, d.bin)
		switch {
		case path == "" && d.required:
			missingRequired = true
			fmt.Printf("  ✗ %-8s %-26s %s\n", d.bin, "MISSING (required)", d.note)
		case path == "":
			fmt.Printf("  · %-8s %-26s %s\n", d.bin, "not installed (optional)", d.note)
		default:
			fmt.Printf("  ✓ %-8s %-26s %s\n", d.bin, firstLine(binVersion(d.bin, d.verArg)), d.note)
			detail := path
			if via != "" {
				detail += "  (" + via + ")"
			}
			fmt.Printf("      %s\n", detail)
			if d.bin == "gh" {
				if gh.Authed(ctx) {
					fmt.Printf("      authenticated\n")
				} else {
					fmt.Printf("      not authenticated — run: gh auth login\n")
				}
			}
		}
	}

	fmt.Println("\nRuntime")
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		fmt.Printf("  ✓ daemon    running (socket %s)\n", core.SocketPath())
	} else {
		fmt.Printf("  · daemon    offline — starts on `amux up`\n")
	}
	if db, err := store.Open(); err == nil {
		repos, _ := db.Repos()
		roots, _ := db.Roots()
		all, _ := db.AllSessions()
		_ = db.Close()
		agents := len(all) - len(roots)
		if agents < 0 {
			agents = 0
		}
		fmt.Printf("  ✓ database  %s\n", core.DBPath())
		fmt.Printf("              %d repos · %d workspaces · %d agents\n", len(repos), len(roots), agents)
	} else {
		fmt.Printf("  ✗ database  %v\n", err)
	}
	if tmuxctl.ServerRunning(ctx) {
		fmt.Printf("  ✓ server    running (tmux -L %s)\n", core.TmuxSocket)
	} else {
		fmt.Printf("  · server    not running — starts on `amux up`\n")
	}

	fmt.Println("\nPaths")
	fmt.Printf("  config   %s\n", core.ConfigDir())
	fmt.Printf("  data     %s\n", core.DataDir())
	fmt.Printf("  state    %s\n", core.StateDir())

	if missingRequired {
		fmt.Println("\n✗ missing a required dependency (see above)")
		return fmt.Errorf("health check failed")
	}
	fmt.Println("\n✓ all required dependencies present")
	return nil
}

// resolveCmd locates bin the way amux actually needs it, reporting how it was
// found: this shell's PATH, the login shell (handles non-lazy nvm/asdf), or the
// running tmux server's environment (where agents are spawned — this is what
// matters with lazy-loaded nvm that a fresh shell can't surface).
func resolveCmd(ctx context.Context, bin string) (path, via string) {
	if p, err := exec.LookPath(bin); err == nil {
		return p, ""
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if out, err := exec.Command(shell, "-lic", "command -v "+bin).Output(); err == nil {
		if p := strings.TrimSpace(string(out)); strings.ContainsRune(p, '/') {
			return p, "via login shell"
		}
	}
	if p := inServerEnv(ctx, bin); p != "" {
		return p, "via tmux server env"
	}
	return "", ""
}

// inServerEnv looks for bin on the PATH of a pane in the isolated tmux server
// (Linux only, via /proc). Agents inherit this environment, so a binary found
// here will launch even if this shell can't see it.
func inServerEnv(ctx context.Context, bin string) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	out, err := tmuxctl.Run(ctx, "list-panes", "-a", "-F", "#{pane_pid}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		pid := strings.TrimSpace(line)
		if pid == "" {
			continue
		}
		data, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(string(data), "\x00") {
			if !strings.HasPrefix(kv, "PATH=") {
				continue
			}
			for _, dir := range strings.Split(kv[len("PATH="):], ":") {
				if dir == "" {
					continue
				}
				p := filepath.Join(dir, bin)
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					return p
				}
			}
		}
		return "" // first pane's PATH is representative
	}
	return ""
}

func binVersion(bin, arg string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, arg).Output()
	return string(out)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
