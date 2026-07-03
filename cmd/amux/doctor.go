package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/gh"
	"amux/internal/git"
)

// cmdDoctor prints a health summary: required/optional CLI dependencies and the
// amux runtime (daemon, database). Exits non-zero if a required dependency is
// missing.
func cmdDoctor() error {
	ctx := context.Background()
	fmt.Print("amux doctor\n\n")

	type dep struct {
		bin, verArg, note string
		required          bool
	}
	deps := []dep{
		{"git", "--version", "bare clones & worktrees", true},
		{"fzf", "--version", "interactive pickers (new workspace/agent)", false},
		{"gh", "--version", "browse & clone GitHub repos", false},
	}
	// Agent binaries come from the registry, so adding a harness adds its check
	// here automatically. The default (first-registered) harness is required; the
	// rest are optional alternates.
	for i, kind := range agent.Kinds() {
		if i == 0 {
			deps = append(deps, dep{kind, "--version", "default agent", true})
		} else {
			deps = append(deps, dep{kind, "--version", "alternate agent", false})
		}
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
	var repos []core.RepoRow
	var roots []core.WorkgroupRow
	statsOK := false
	if !daemonUp {
		fmt.Printf("  · database  %s (start the daemon to read stats)\n", core.DBPath())
	} else if r, rt, err := doctorStats(); err != nil {
		fmt.Printf("  ✗ database  %v\n", err)
	} else {
		repos, roots, statsOK = r, rt, true
		agents := 0
		for _, wg := range roots {
			agents += len(wg.Agents)
		}
		fmt.Printf("  ✓ database  %s\n", core.DBPath())
		fmt.Printf("              %d repos · %d workspaces · %d agents\n", len(repos), len(roots), agents)
	}

	// Reconcile the store against on-disk worktree dirs and branches: surface
	// leftovers (a crash or a bug that removed a store row but not its dir/branch)
	// and the reverse (a store row whose dir vanished). Reads only through the
	// daemon's session model, never the store.
	fmt.Println("\nReconciliation")
	if !statsOK {
		fmt.Printf("  · sessions  (start the daemon to reconcile store vs disk)\n")
	} else {
		reconcileSessions(ctx, repos, roots)
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

// branchRef is one amux/* branch found in a repo's bare clone.
type branchRef struct{ Repo, Branch string }

// reconcileSessions compares the store's sessions (fetched via the daemon) with
// what's on disk — the worktree dirs under SessionsDir and the amux/* branches in
// each repo — and prints drift in both directions. Reads only through the daemon's
// session model, never the store.
func reconcileSessions(ctx context.Context, repos []core.RepoRow, roots []core.WorkgroupRow) {
	var disk []branchRef
	for _, repo := range repos {
		gitDir := filepath.Join(core.ReposDir(), repo.Name+".git")
		for _, b := range git.ListBranches(ctx, gitDir, "amux/*") {
			disk = append(disk, branchRef{Repo: repo.Name, Branch: b})
		}
	}
	orphanDirs, missingDirs, orphanBranches := reconcile(core.SessionsDir(), roots, disk)

	if len(orphanDirs)+len(missingDirs)+len(orphanBranches) == 0 {
		fmt.Printf("  ✓ sessions  store and disk agree\n")
		return
	}
	printCapped("worktree dir with no session", orphanDirs, func(d string) string {
		return filepath.Join(core.SessionsDir(), d)
	})
	printCapped("session with no worktree dir", missingDirs, func(s string) string { return s })
	printCapped("amux branch with no session", orphanBranches, func(s string) string { return s })
	fmt.Printf("  (leftovers from an interrupted delete/move; safe to remove by hand)\n")
}

// printCapped prints up to reconcileCap findings of one kind, then a "+N more"
// summary — accumulated leftovers can run to dozens, and a doctor report should
// flag the drift without burying the rest of the health summary.
func printCapped(label string, items []string, render func(string) string) {
	const reconcileCap = 10
	for i, it := range items {
		if i == reconcileCap {
			fmt.Printf("  ⚠ … and %d more %s\n", len(items)-reconcileCap, label+"(s)")
			break
		}
		fmt.Printf("  ⚠ %s: %s\n", label, render(it))
	}
}

// reconcile is the pure core of the session reconciliation: given the sessions
// dir, the known workgroups, and the amux/* branches found on disk, it returns the
// drift in each direction (sorted). Worktree dirs are matched by agent id (the
// dir's leaf name), so an agent moved to another workgroup — whose dir still lives
// under its original root — is recognized rather than flagged. Branches are
// matched against each agent's stored branch (core.AgentRow.Branch), so a branch
// left behind by a deleted agent surfaces even if the scheme later changes.
func reconcile(sessionsDir string, roots []core.WorkgroupRow, disk []branchRef) (orphanDirs, missingDirs, orphanBranches []string) {
	knownAgent := map[string]bool{}
	knownBranch := map[string]bool{}
	for _, wg := range roots {
		for _, a := range wg.Agents {
			knownAgent[a.ID] = true
			if a.Branch != "" {
				knownBranch[a.Branch] = true
			}
		}
	}

	seenDir := map[string]bool{}
	rootEnts, _ := os.ReadDir(sessionsDir)
	for _, re := range rootEnts {
		if !re.IsDir() {
			continue
		}
		agentEnts, _ := os.ReadDir(filepath.Join(sessionsDir, re.Name()))
		for _, ae := range agentEnts {
			if !ae.IsDir() {
				continue
			}
			seenDir[ae.Name()] = true
			if !knownAgent[ae.Name()] {
				orphanDirs = append(orphanDirs, filepath.Join(re.Name(), ae.Name()))
			}
		}
	}
	for id := range knownAgent {
		if !seenDir[id] {
			missingDirs = append(missingDirs, id)
		}
	}
	for _, ref := range disk {
		// A branch is accounted for if it's some agent's stored branch, or — even
		// when that field is blank (older sessions) — if the agent id encoded in the
		// branch is a live agent. Only a branch that maps to no session is a leftover.
		if knownBranch[ref.Branch] {
			continue
		}
		if id := agentIDFromBranch(ref.Branch); id != "" && knownAgent[id] {
			continue
		}
		orphanBranches = append(orphanBranches, ref.Repo+" · "+ref.Branch)
	}

	sort.Strings(orphanDirs)
	sort.Strings(missingDirs)
	sort.Strings(orphanBranches)
	return orphanDirs, missingDirs, orphanBranches
}

// agentIDFromBranch extracts the agent id from an amux branch (amux/<root>-<agent>
// → <agent>), or "" for a legacy amux/<root> branch that names no agent. This lets
// reconciliation match a branch to a live agent by id even when the session's
// stored branch is blank.
func agentIDFromBranch(branch string) string {
	b := strings.TrimPrefix(branch, "amux/")
	if i := strings.LastIndex(b, "-"); i >= 0 {
		return b[i+1:]
	}
	return ""
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
