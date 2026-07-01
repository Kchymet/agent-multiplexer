package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/gh"
)

// cmdDoctor prints a health summary: required/optional CLI dependencies and the
// amux runtime (daemon, database). Exits non-zero if a required dependency is
// missing.
func cmdDoctor() error {
	ctx := context.Background()
	fmt.Print("amux doctor\n\n")

	deps := []struct {
		bin, verArg, note string
		required          bool
	}{
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
	daemonUp := false
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		daemonUp = true
		fmt.Printf("  ✓ daemon    running (socket %s)\n", core.SocketPath())
	} else {
		fmt.Printf("  · daemon    offline — starts on `amux`\n")
	}
	// The daemon is the sole owner of the store, so ask it for the counts over
	// the socket rather than opening the database here. With the daemon offline
	// there's nothing to read — just show where the database lives.
	if !daemonUp {
		fmt.Printf("  · database  %s (start the daemon to read stats)\n", core.DBPath())
	} else if repos, roots, err := doctorStats(); err != nil {
		fmt.Printf("  ✗ database  %v\n", err)
	} else {
		agents := 0
		for _, r := range roots {
			agents += len(r.Agents)
		}
		fmt.Printf("  ✓ database  %s\n", core.DBPath())
		fmt.Printf("              %d repos · %d workspaces · %d agents\n", len(repos), len(roots), agents)
	}
	fmt.Println("\nPaths")
	fmt.Printf("  data     %s\n", core.DataDir())
	fmt.Printf("  state    %s\n", core.StateDir())

	if missingRequired {
		fmt.Println("\n✗ missing a required dependency (see above)")
		return fmt.Errorf("health check failed")
	}
	fmt.Println("\n✓ all required dependencies present")
	return nil
}

// doctorStats reads the repo and workgroup counts from the daemon (the store
// owner) for the health summary. It reuses the same read models the CLI's
// `repo ls` / `session ls` go through, so doctor never opens the store itself.
func doctorStats() ([]core.RepoRow, []core.WorkgroupRow, error) {
	repos, err := queryRepos()
	if err != nil {
		return nil, nil, err
	}
	roots, err := querySessions()
	if err != nil {
		return nil, nil, err
	}
	return repos, roots, nil
}

// resolveCmd locates bin the way amux actually needs it, reporting how it was
// found: this shell's PATH, or the login shell (handles non-lazy nvm/asdf).
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
	return "", ""
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
