package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/gh"
	"amux/internal/git"
	"amux/internal/provider"
	"amux/internal/providercfg"
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
		{"fzf", "--version", "interactive pickers (new workgroup/agent)", false},
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
		fmt.Printf("              %d repos · %d workgroups · %d agents\n", len(repos), len(roots), agents)
	}

	// Reconcile the store against on-disk worktree dirs and branches: surface
	// leftovers (a crash or a bug that removed a store row but not its dir/branch)
	// and the reverse (a store row whose dir vanished). Reads only through the
	// daemon's session model, never the store.
	fmt.Println("\nReconciliation")
	if !statsOK {
		fmt.Printf("  · agents    (start the daemon to reconcile store vs disk)\n")
	} else {
		reconcileSessions(ctx, repos, roots)
	}

	// Harness surface drift: each harness self-checks its load-bearing integration
	// with an unversioned upstream CLI (Claude's project-dir path munge) and reports
	// mismatches loudly, instead of resume/status/capture degrading in silence.
	fmt.Println("\nAgent transcripts")
	anyDrift := false
	for _, h := range agent.Harnesses() {
		findings := h.Doctor()
		if len(findings) == 0 {
			continue
		}
		anyDrift = true
		for _, f := range findings {
			fmt.Printf("  ⚠ %-8s %s\n", h.Kind(), f)
		}
	}
	if !anyDrift {
		fmt.Printf("  ✓ claude    transcript folders match their working directories\n")
	} else {
		fmt.Println("  Resume or transcript capture may be affected. Check whether the working directory moved;")
		fmt.Println("  otherwise report these paths as an amux compatibility issue. Keep the transcripts.")
	}

	// Sandbox config: each agent runs against a private copy of the user's harness
	// config; edits an agent made to its copy await the user's decision to
	// propagate or discard. Counted through the daemon (it resolves the agents).
	fmt.Println("\nSandbox config")
	if !daemonUp {
		fmt.Printf("  · agents    (start the daemon to compare config copies with your templates)\n")
	} else if agents, edits, err := sandboxDriftSummary(); err != nil {
		fmt.Printf("  ⚠ agents    could not compare config copies: %v\n", err)
	} else if edits == 0 {
		fmt.Printf("  ✓ agents    no agent-side config differences awaiting review\n")
	} else {
		fmt.Printf("  ⚠ agents    %d config file difference%s across %d agent%s — review: amux sandbox drift\n",
			edits, plural(edits), agents, plural(agents))
		fmt.Println("              Counts files per agent, not editing actions; harnesses can update their own config.")
		fmt.Println("              Nothing is promoted automatically. Use `amux sandbox promote/reset <id> <path>` after review.")
	}

	// Browser: agents open links (gh --web, Claude's login) through $BROWSER, and
	// a value that this shell resolves can still be unreachable from a pane — the
	// sandbox hides $HOME, and a WSL $BROWSER under /mnt/c is a per-machine path.
	fmt.Println("\nBrowser")
	for _, l := range checkBrowser(probeBrowser(ctx, daemonUp)) {
		fmt.Println(l)
	}

	// Provider mode: opt-in, so a machine that never registered says so quietly
	// rather than failing the check. What it reports is the whole chain — config,
	// credential, service, and whether the loop actually reached an orchestrator —
	// because a provider that is installed but silently rejected looks, from the
	// outside, exactly like one that works.
	fmt.Println("\nProvider")
	reportProvider()

	// Terminal hotkeys: the native TUI's Alt/Option bindings only work when the
	// terminal encodes Option as ESC-prefixed Meta. Best-effort and never fatal;
	// silent when there's nothing to say (non-macOS or an unrecognized terminal).
	if lines := checkHotkeys(os.Getenv, iterm2OptionKeySends); len(lines) != 0 {
		fmt.Println("\nTerminal")
		for _, l := range lines {
			fmt.Println(l)
		}
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
		for _, b := range git.ListBranches(ctx, gitDir, core.BranchPrefix+"*") {
			disk = append(disk, branchRef{Repo: repo.Name, Branch: b})
		}
	}
	orphanDirs, missingDirs, orphanBranches := reconcile(core.SessionsDir(), roots, disk)

	if len(orphanDirs)+len(missingDirs)+len(orphanBranches) == 0 {
		fmt.Printf("  ✓ agents    store and disk agree\n")
		return
	}
	printCapped("agent directory not tracked by amux", orphanDirs, func(d string) string {
		return filepath.Join(core.SessionsDir(), d)
	})
	printCapped("tracked agent missing its directory", missingDirs, func(s string) string { return s })
	printCapped("branch not linked to a tracked agent", orphanBranches, func(s string) string { return s })
	if len(missingDirs) > 0 {
		fmt.Println("  Missing directories can prevent agents from resuming. Check moved/deleted paths with: amux workgroup ls")
	}
	if len(orphanDirs)+len(orphanBranches) > 0 {
		fmt.Println("  Untracked items may contain work you want to keep. Inspect files and unmerged commits before removing them.")
	}
}

// printCapped prints up to reconcileCap findings of one kind, then a "+N more"
// summary — accumulated leftovers can run to dozens, and a doctor report should
// flag the drift without burying the rest of the health summary.
func printCapped(label string, items []string, render func(string) string) {
	const reconcileCap = 10
	for i, it := range items {
		if i == reconcileCap {
			fmt.Printf("  ⚠ … and %d more (%d total): %s\n", len(items)-reconcileCap, len(items), label)
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
			// Root sessions keep their own config alongside child agent dirs.
			if !ae.IsDir() || strings.HasPrefix(ae.Name(), ".") {
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

// agentIDFromBranch extracts the agent id from an amux branch (amux/<root>-<agent>[-<description>]
// → <agent>), or "" for a legacy amux/<root> branch that names no agent. This lets
// reconciliation match a branch to a live agent by id even when the session's
// stored branch is blank.
func agentIDFromBranch(branch string) string {
	b, ok := strings.CutPrefix(branch, core.BranchPrefix)
	if !ok {
		return ""
	}
	if _, tail, ok := strings.Cut(b, "-"); ok {
		id, _, _ := strings.Cut(tail, "-")
		return id
	}
	return ""
}

// iTerm2's per-profile "Option Key Sends" setting: 0 Normal (Option types
// composed characters like ¬ and the TUI's Alt hotkeys never fire), 1 Meta,
// 2 Esc+.
const optionKeyNormal = 0

// checkHotkeys reports whether the outer terminal likely delivers the native
// TUI's Alt/Option hotkeys (Alt+h/l/a/q, Alt+1..3), which require Option to be
// encoded as ESC-prefixed Meta. The outer terminal is detected via LC_TERMINAL
// and TERM_PROGRAM — both survive into tmux sessions, so this sees through a
// tmux layer. For iTerm2 the verdict is made definitive by reading the active
// profile's (ITERM_PROFILE) "Option Key Sends" value via optionKeySends; if
// that read fails the check degrades to an informational hint. Returns the
// lines to print, or nil when there is nothing to say (non-macOS or an
// unrecognized terminal).
func checkHotkeys(getenv func(string) string, optionKeySends func(profile string) (int, error)) []string {
	term := getenv("LC_TERMINAL")
	if term == "" {
		term = getenv("TERM_PROGRAM")
	}
	switch term {
	case "iTerm2", "iTerm.app":
		const fix = `iTerm2 → Settings → Profiles → Keys → General → set "Left Option (⌥) key" to "Esc+"`
		v, err := optionKeySends(getenv("ITERM_PROFILE"))
		switch {
		case err != nil:
			return []string{
				"  · hotkeys   iTerm2 detected but its profile settings are unreadable",
				"              if Alt/Option hotkeys (Alt+h/l/a/q, Alt+1..3) don't work: " + fix,
			}
		case v == optionKeyNormal:
			return []string{
				`  ⚠ hotkeys   iTerm2 Option key is "Normal" — Alt/Option hotkeys (Alt+h/l/a/q, Alt+1..3) never reach the TUI`,
				"              fix: " + fix,
			}
		default:
			return []string{"  ✓ hotkeys   iTerm2 sends Option as Esc+/Meta — Alt/Option hotkeys work"}
		}
	case "Apple_Terminal":
		return []string{
			"  · hotkeys   Terminal.app swallows Alt/Option hotkeys unless Option acts as Meta",
			`              enable: Terminal → Settings → Profiles → Keyboard → "Use Option as Meta key"`,
		}
	}
	return nil
}

// iterm2OptionKeySends reads the "Option Key Sends" value for the named iTerm2
// profile from ~/Library/Preferences/com.googlecode.iterm2.plist via PlistBuddy.
// Best-effort: any failure (no PlistBuddy, unreadable plist, profile or key not
// found) returns an error and doctor falls back to a hint instead of a warning.
func iterm2OptionKeySends(profile string) (int, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0, err
	}
	plist := filepath.Join(home, "Library", "Preferences", "com.googlecode.iterm2.plist")
	// Profiles live in the "New Bookmarks" array; find the active one by name.
	// The array only holds user-created profiles, so a capped scan is plenty.
	for i := 0; i < 64; i++ {
		name, err := plistBuddyPrint(plist, fmt.Sprintf(":New Bookmarks:%d:Name", i))
		if err != nil {
			break // ran off the end of the array, or the plist is unreadable
		}
		if name != profile {
			continue
		}
		v, err := plistBuddyPrint(plist, fmt.Sprintf(":New Bookmarks:%d:Option Key Sends", i))
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(v)
	}
	return 0, fmt.Errorf("profile %q not found in %s", profile, plist)
}

func plistBuddyPrint(plist, entry string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// PlistBuddy re-tokenizes the -c string on whitespace, so an entry path with
	// a space ("New Bookmarks") must be quoted inside the command itself.
	out, err := exec.CommandContext(ctx, "/usr/libexec/PlistBuddy", "-c", "Print '"+entry+"'", plist).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

// reportProvider prints doctor's Provider section: the config `amux provide
// install` wrote, the token file's mode, the user service, and the provider
// loop's own last registration and heartbeat. Never fatal — provider mode is
// opt-in, so "not configured" is a normal, healthy state.
func reportProvider() {
	cfg, err := providercfg.Load()
	if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("  · config    not configured — `amux provide install --orchestrator host:port --token-file <path>`\n")
		return
	}
	if err != nil {
		fmt.Printf("  ✗ config    %v\n", err)
		return
	}
	fmt.Printf("  ✓ config    %s\n", providercfg.Path())
	detail := "orchestrator " + cfg.Orchestrator
	if cfg.Name != "" {
		detail += " · name " + cfg.Name
	}
	if cfg.PublishSessions {
		feature := " · sessions"
		if cfg.ReadOnlySessions {
			feature += " (read-only)"
		}
		if cfg.RuntimeEvents {
			feature += " + runtime-events"
		}
		detail += feature
	}
	fmt.Printf("              %s\n", detail)
	reportProviderToken(cfg.TokenFile)
	reportProviderService()
	reportProviderStatus()
}

// reportProviderToken checks the bearer credential is present and not readable
// by anyone else — the service reads it unattended, so nobody is watching for a
// stray 0644.
func reportProviderToken(path string) {
	if path == "" {
		fmt.Printf("  ⚠ token     no token-file in the config; the provider falls back to $AMUX_PROVIDER_TOKEN\n")
		return
	}
	st, err := os.Stat(path)
	switch {
	case err != nil:
		fmt.Printf("  ✗ token     %v\n", err)
	case st.Mode().Perm() != 0o600:
		fmt.Printf("  ⚠ token     %s is mode %04o, want 0600 — run: chmod 600 %s\n", path, st.Mode().Perm(), path)
	default:
		fmt.Printf("  ✓ token     %s (mode 0600)\n", path)
	}
}

// reportProviderService reports the user service and, on Linux, whether linger
// keeps it alive past logout.
func reportProviderService() {
	svc, err := providercfg.Service()
	if err != nil {
		fmt.Printf("  · service   %v — run `amux provide <addr>` in the foreground\n", err)
		return
	}
	p := svc.Probe()
	switch {
	case !p.Installed:
		fmt.Printf("  · service   no %s unit installed — run: amux provide install\n", svc.Kind())
		return
	case p.Active:
		fmt.Printf("  ✓ service   %s · %s\n", svc.Kind(), p.Detail)
	default:
		fmt.Printf("  ⚠ service   %s · %s (not running)\n", svc.Kind(), p.Detail)
	}
	enabledAtBoot := "enabled at login"
	if !p.Enabled {
		enabledAtBoot = "NOT enabled at login"
	}
	fmt.Printf("              %s · %s\n", svc.Path(), enabledAtBoot)
	if enabled, known := providercfg.Linger(); known && !enabled {
		fmt.Printf("  ⚠ linger    off — the service stops when your last session closes; run: %s\n", providercfg.LingerHint())
	}
}

// reportProviderStatus reads the status file the provider loop writes. It is the
// only way to tell a registered provider from one that is running but has never
// been accepted — the difference the log would show if anyone were reading it.
func reportProviderStatus() {
	st, err := provider.ReadStatus(provider.StatusPath())
	if errors.Is(err, os.ErrNotExist) {
		fmt.Printf("  · status    the provider has not run yet (no %s)\n", provider.StatusPath())
		return
	}
	if err != nil {
		fmt.Printf("  ✗ status    %v\n", err)
		return
	}
	mark := "·"
	switch st.State {
	case provider.StateRegistered:
		mark = "✓"
	case provider.StateRejected:
		mark = "✗"
	case provider.StateDisconnected, provider.StateDialing:
		mark = "⚠"
	}
	// A "registered" record whose process is gone is a lie the file cannot
	// retract on its own (SIGKILL, a reboot); say so instead of repeating it.
	live := processAlive(st.PID)
	if !live && st.State != provider.StateStopped {
		mark = "⚠"
	}
	fmt.Printf("  %s status    %s (%s)\n", mark, st.State, ago(st.UpdatedAt))
	line := fmt.Sprintf("registered %s · heartbeat %s", ago(st.RegisteredAt), ago(st.HeartbeatAt))
	if st.ProviderID != "" {
		line = "providerId " + st.ProviderID + " · " + line
	}
	fmt.Printf("              %s · %d panes\n", line, st.Panes)
	if !live && st.State != provider.StateStopped {
		fmt.Printf("              pid %d is gone — the provider died without recording a stop\n", st.PID)
	}
	if st.LastError != "" {
		fmt.Printf("              last error: %s\n", st.LastError)
	}
}

// ago renders a timestamp as a compact age, or "never" for a zero time.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}
