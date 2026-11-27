package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/buildinfo"
	"amux/internal/daemon"
	"amux/internal/selfupdate"
)

// cmdVersionArgs dispatches `amux version [--short|--manifest]`. --short prints
// only this binary's product version and never contacts the daemon (self-update
// uses it to identify a binary on disk); --manifest prints the build's
// compatibility contracts as the release manifest JSON.
func cmdVersionArgs(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "--short", "-s":
			fmt.Println(buildinfo.Version)
			return nil
		case "--manifest":
			b, err := json.MarshalIndent(daemon.CurrentManifest(), "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		case "-h", "--help", "help":
			return showHelp("version")
		default:
			return fmt.Errorf("unknown version flag %q (want --short or --manifest)", args[0])
		}
	}
	return cmdVersion()
}

type updateFlags struct {
	check, force, restartDaemon bool
}

func parseUpdateFlags(args []string) (updateFlags, error) {
	var f updateFlags
	for _, a := range args {
		switch a {
		case "--check", "-n":
			f.check = true
		case "--force", "-f":
			f.force = true
		case "--restart-daemon":
			f.restartDaemon = true
		case "-h", "--help", "help":
			return f, errUpdateHelp
		default:
			return f, fmt.Errorf("unknown update flag %q", a)
		}
	}
	return f, nil
}

var errUpdateHelp = errors.New("help")

// updateDeps are the seams cmdUpdate uses, replaced in tests.
type updateDeps struct {
	newUpdater    func() (*selfupdate.Updater, error)
	versions      func() versionReport
	restartDaemon func() error
	out           func(format string, a ...any)
}

var defaultUpdateDeps = updateDeps{
	newUpdater:    daemon.NewUpdater,
	versions:      func() versionReport { return collectVersionReport() },
	restartDaemon: daemonRestart,
	out:           func(format string, a ...any) { fmt.Printf(format, a...) },
}

func cmdUpdate(args []string) error {
	f, err := parseUpdateFlags(args)
	if errors.Is(err, errUpdateHelp) {
		return showHelp("update")
	}
	if err != nil {
		return err
	}
	if restrictedSessionClient(os.Getenv) {
		return fmt.Errorf("refusing to update amux from inside an amux agent; run amux update in a host shell")
	}
	return runUpdate(context.Background(), f, defaultUpdateDeps)
}

func runUpdate(ctx context.Context, f updateFlags, deps updateDeps) error {
	out := deps.out
	u, err := deps.newUpdater()
	if err != nil {
		return err
	}
	running := u.Running.Version
	unlock, err := u.Store.Lock(30 * time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	st, _ := u.Store.Load()

	m, err := u.Source.Latest(ctx)
	st.LastCheck = time.Now()
	if err != nil {
		st.LastError = err.Error()
		_ = u.Store.Save(st)
		return fmt.Errorf("check latest release: %w", err)
	}
	st.Latest, st.LastError = m.Version, ""
	_ = u.Store.Save(st)

	installed, probeErr := u.InstalledVersion(ctx)
	if probeErr != nil {
		installed = running
	}
	dev := !buildinfo.IsRelease(installed)
	current := installed
	newer := selfupdate.Newer(m.Version, current)
	out("installed %s  (%s)\n", current, u.Target)
	out("latest    %s\n", m.Version)
	if installed != running {
		out("running   %s  (this process)\n", running)
	}
	if f.check {
		switch {
		case dev:
			out("this is a development build; amux update --force replaces it with %s\n", m.Version)
		case newer:
			out("update available: run amux update\n")
		default:
			out("up to date\n")
		}
		if st.Staged != nil {
			out("staged    %s (installs at the next daemon restart)\n", st.Staged.Version)
		}
		return nil
	}
	if dev && !f.force {
		return fmt.Errorf("%s is a development build; rerun with --force to replace it with release %s", installed, m.Version)
	}
	if !newer && !f.force {
		out("amux %s is up to date\n", current)
		return nil
	}
	if compat := selfupdate.Compatibility(m, u.Running); compat == selfupdate.CompatNever {
		return fmt.Errorf("refusing to install %s: it supports database schema %d–%d, but this installation is at schema %d",
			m.Version, m.DatabaseMinSchema, m.DatabaseMaxSchema, u.Running.DatabaseMaxSchema)
	}
	if err := selfupdate.Writable(u.Target); err != nil {
		return err
	}
	out("downloading %s for this platform…\n", m.Version)
	staged, err := u.Fetch(ctx, m)
	if err != nil {
		return err
	}
	staged.Forced = f.force && dev
	out("verified SHA256 %s\n", staged.SHA256)

	// Decide whether the daemon can be restarted now. Unknown agent state
	// counts as "agents may be running".
	r := deps.versions()
	live := -1
	if r.Connected && r.QueryErr == nil && r.Runtime.LiveAgents != nil {
		live = *r.Runtime.LiveAgents
	}
	daemonUp := r.Connected
	compat := selfupdate.Compatibility(m, u.Running)
	if daemonUp && r.QueryErr == nil {
		// Compare against what the daemon actually speaks, not this CLI.
		running := u.Running
		running.DaemonProtocol = r.Runtime.DaemonProtocol
		running.DatabaseMaxSchema = r.Runtime.DatabaseMaxSchema
		compat = selfupdate.Compatibility(m, running)
	}
	restartNow := daemonUp && (live == 0 || f.restartDaemon)

	if daemonUp && !restartNow && compat != selfupdate.CompatHot {
		// Replacing the binary under a running daemon whose protocol it does
		// not speak would strand every new CLI; keep it staged instead.
		st.Staged = staged
		if err := u.Store.Save(st); err != nil {
			return err
		}
		out("staged %s: it changes the daemon protocol or schema, and %s.\n", m.Version, agentsPhrase(live))
		out("It installs when no agents are running, at the next amux daemon restart, or now with amux update --restart-daemon.\n")
		return nil
	}
	if err := u.Apply(&st, staged); err != nil {
		st.LastError = err.Error()
		_ = u.Store.Save(st)
		return err
	}
	st.Staged, st.Pending = nil, ""
	if err := u.Store.Save(st); err != nil {
		return err
	}
	out("installed amux %s at %s\n", m.Version, u.Target)
	if staged.Shim != "" {
		out("refreshed shell shim %s\n", u.ShimPath)
	}
	switch {
	case !daemonUp && r.ConnectErr != nil && !daemonMayStart(r.ConnectErr):
		out("daemon state unknown (%v); run amux daemon restart to load %s\n", r.ConnectErr, m.Version)
	case !daemonUp:
		out("daemon not running; it starts on %s next launch\n", m.Version)
	case restartNow:
		unlock() // daemon restart may apply a staged update under the same lock
		out("restarting daemon to load %s…\n", m.Version)
		if err := deps.restartDaemon(); err != nil {
			return fmt.Errorf("installed %s, but restarting the daemon failed: %w", m.Version, err)
		}
	default:
		out("daemon keeps running %s because %s; it restarts into %s on its own once none are running\n",
			r.Runtime.DaemonVersion, agentsPhrase(live), m.Version)
		out("(or run amux daemon restart now: running agents are stopped and resumed)\n")
	}
	return nil
}

func agentsPhrase(live int) string {
	switch {
	case live < 0:
		return "the daemon's agents could not be counted"
	case live == 1:
		return "1 agent is running"
	}
	return fmt.Sprintf("%d agents are running", live)
}

// applyStagedBeforeStart installs a staged (protocol-changing) release while
// no daemon is running: the "next restart" path of a staged update. Failures
// are reported, never fatal — the daemon still starts on the current binary.
func applyStagedBeforeStart() {
	u, err := daemon.NewUpdater()
	if err != nil {
		return
	}
	st, err := u.Store.Load()
	if err != nil || st.Staged == nil {
		return
	}
	version, err := u.ApplyPendingStaged()
	if err != nil {
		fmt.Fprintf(os.Stderr, "amux: warning: staged update not installed: %v\n", err)
		return
	}
	if version != "" {
		fmt.Fprintf(os.Stderr, "amux: installed staged update %s\n", version)
	}
}

// printPendingUpdate notes on stderr that an installed or staged release has
// not been loaded by the daemon yet (used by amux ls).
func printPendingUpdate() {
	r := collectVersionReport()
	if line, pending := updateStatusLine(r); pending {
		fmt.Fprintln(os.Stderr, "update: "+line)
	}
	for _, line := range harnessRestartLines(r) {
		fmt.Fprintln(os.Stderr, "harness: "+line)
	}
}

// harnessRestartLines describes running sessions the daemon will restart onto
// a newer amux-managed harness version once they are idle between turns.
func harnessRestartLines(r versionReport) []string {
	if !r.Connected || r.QueryErr != nil {
		return nil
	}
	var out []string
	for _, h := range r.Runtime.HarnessRestarts {
		out = append(out, fmt.Sprintf("%s restart pending for harness update (%s %s → %s): %s", h.Session, h.Kind, h.From, h.To, h.Reason))
	}
	return out
}

// updateStatusLine summarizes self-update state for version/doctor/status
// without network access. It returns "" when there is nothing to say.
func updateStatusLine(r versionReport) (line string, pending bool) {
	st, err := daemon.UpdateStore().Load()
	if err != nil {
		return fmt.Sprintf("state unreadable (%v)", err), false
	}
	if r.Connected && r.QueryErr == nil && r.Runtime.UpdatePending != "" {
		return fmt.Sprintf("%s pending — daemon %s: %s", r.Runtime.UpdatePending, r.Runtime.DaemonVersion, r.Runtime.UpdatePendingReason), true
	}
	if st.Staged != nil && selfupdate.Newer(st.Staged.Version, r.CLI) {
		return fmt.Sprintf("%s staged — installs at the next daemon restart (or amux update --restart-daemon)", st.Staged.Version), true
	}
	if r.Connected && r.QueryErr == nil && st.Installed != "" &&
		selfupdate.Newer(st.Installed, r.Runtime.DaemonVersion) && st.Installed == r.CLI {
		return fmt.Sprintf("%s installed — daemon still runs %s until it restarts", st.Installed, r.Runtime.DaemonVersion), true
	}
	auto := "automatic updates on"
	if on, err := amuxcfg.ResolveAutoUpdate(); err != nil || !on {
		auto = "automatic updates off (update.auto)"
	} else if selfupdate.EnvDisabled(os.Getenv) {
		auto = "automatic updates off (" + selfupdate.DisableEnv + ")"
	} else if !buildinfo.IsRelease(r.CLI) {
		auto = "development build: never auto-updated (amux update --force installs a release)"
	}
	var parts []string
	if st.Latest != "" && selfupdate.Newer(st.Latest, r.CLI) {
		parts = append(parts, fmt.Sprintf("%s available (amux update)", st.Latest))
	} else if !st.LastCheck.IsZero() {
		parts = append(parts, fmt.Sprintf("checked %s ago", time.Since(st.LastCheck).Round(time.Minute)))
	}
	parts = append(parts, auto)
	if st.LastError != "" {
		parts = append(parts, "last error: "+st.LastError)
	}
	return strings.Join(parts, "; "), false
}
