// Command amux is an AI-native terminal control plane: it runs agent sessions as
// daemon-owned processes and shows a persistent, interactive dashboard of every
// local and cloud agent.
//
// Subcommands:
//
//	(bare)   open the native dashboard TUI
//	daemon   run the polling/serving daemon (foreground)
//	agent    self-reporting run by an agent about itself (status/hook/name/done)
//	status   print current workgroups as text and exit
//	top      per-session CPU/memory and amux's own overhead
//	queue    the user question queue: list, answer, dismiss, acknowledge
//	version  print CLI, daemon, and database versions
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/daemonstatus"
	"amux/internal/engine/local"
	"amux/internal/logrotate"
	"amux/internal/nativetui"
	"amux/internal/procstat"
	_ "amux/internal/termbg"
)

func main() {
	if filepath.Base(os.Args[0]) == "security" {
		os.Exit(credentialSecurity(os.Args[1:], os.Stdin, os.Stdout))
	}
	if filepath.Base(os.Args[0]) == "gh" {
		os.Exit(credentialGitHub(os.Args[1:]))
	}
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr, stdioIsTerminal))
}

// daemonSubcommands is the daemon lifecycle dispatch, keyed by verb (aliases
// share a handler). The "" key is bare `amux daemon`: run the daemon in the
// foreground, which is how ensureDaemon spawns it; the rest control an
// already-running detached daemon. It's a table so the contract test can check
// the advertised verbs without executing them — start/stop/restart drive real
// processes.
var daemonSubcommands = map[string]func() error{
	"":        daemonRun,
	"stop":    func() error { return daemonStop(true) },
	"down":    func() error { return daemonStop(true) },
	"start":   daemonStartSelf,
	"up":      daemonStartSelf,
	"restart": daemonRestart,
	"reload":  daemonRestart,
}

// startupDial is the single-attempt socket probe used by client and lifecycle
// paths. Keeping it replaceable lets regressions prove that a denied client does
// not fall through to process creation without manufacturing a host-level Unix
// socket permission failure.
var startupDial = daemon.Dial

// daemonCommand is the detached spawn boundary, replaceable only so tests can
// fail on any attempted exec after a denied probe.
var daemonCommand = exec.Command

// startupProbe reports whether a daemon accepts an authenticated connection
// now. Lifecycle waits poll it; tests replace it to model a slow daemon.
var startupProbe = func() error {
	c, err := startupDial()
	if err == nil {
		_ = c.Close()
	}
	return err
}

// daemonStopBudget bounds how long stop/restart waits for the old daemon to
// release its singleton. Teardown deliberately lets every busy agent finish
// its turn (engine shutdown grace) before SIGTERM→SIGKILL (kill grace); the
// client must outlast that, plus supervisor and authority teardown, or every
// restart issued while an agent works is reported as a failed shutdown.
var daemonStopBudget = func() time.Duration { return local.ShutdownBudget() + 20*time.Second }

// daemonStartupBudget bounds how long a spawned daemon may take to accept its
// first connection while it is still running. Startup reconciles every
// session mailbox before listening, which grows with the number of agents.
var daemonStartupBudget = 90 * time.Second

// sessionContextRestricted is fixed-path namespace provenance, replaceable only
// by tests. A valid context proves a restricted session; an unreadable or
// invalid context fails closed. Only a proven ENOENT means the fixed mount is
// absent. Environment variables remain hints and cannot remove this guard.
var sessionContextRestricted = func() bool {
	return restrictedBySessionContext(access.LoadSessionContext)
}

// protectedHostStartup validates possession from a path that HOME/XDG cannot
// redirect. Automatic startup is allowed only after a daemon has provisioned
// this host credential. Explicit `amux daemon start` is the bootstrap path.
var protectedHostStartup = func() error {
	dir, err := access.ProtectedHostCredentialDir()
	if err != nil {
		return err
	}
	return checkProtectedHostCredential(dir, time.Now())
}

// checkProtectedHostCredential reports why the credential in dir cannot
// authorize automatic startup. Expiry is called out explicitly because it is
// the one condition `amux daemon start` recovers from on its own; a lapsed
// credential is not a forged or foreign one.
func checkProtectedHostCredential(dir string, now time.Time) error {
	credential, err := access.LoadCredential(dir)
	if err != nil {
		return err
	}
	if credential.Kind != access.SubjectHost || credential.SubjectID != access.LocalHostSubject {
		return access.ErrInvalidCredential
	}
	if credential.NotAfter <= now.UnixMilli() {
		return fmt.Errorf("host credential expired at %s: %w",
			time.UnixMilli(credential.NotAfter).UTC().Format(time.RFC3339), access.ErrExpired)
	}
	return nil
}

// cmdDaemon dispatches the daemon lifecycle.
func cmdDaemon(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if isHelpFlag(sub) {
		return showHelp("daemon")
	}
	run, ok := daemonSubcommands[sub]
	if !ok {
		return fmt.Errorf("unknown daemon subcommand %q (want stop|start|restart)", sub)
	}
	return run()
}

// daemonStartSelf starts a detached daemon running this binary.
func daemonStartSelf() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return daemonStart(self)
}

// isHelpFlag reports whether a subcommand word is a request for the command's
// own help, so every namespace (repo, workgroup, do, agent, daemon) answers
// --help/-h/help the same way.
func isHelpFlag(sub string) bool {
	switch sub {
	case "help", "-h", "--help":
		return true
	}
	return false
}

// daemonRun runs the daemon in the foreground until signalled. It is the process
// ensureDaemon/daemonStart spawn; it owns the engine, so its clean shutdown
// (SIGTERM) stops the agents it hosts.
func daemonRun() error {
	// Single instance: if the socket already answers, another daemon owns it.
	if c, err := startupDial(); err == nil {
		_ = c.Close()
		return nil
	} else if daemonAccessDenied(err) {
		return fmt.Errorf("daemon state unknown: access denied at %s; not starting: %w", core.SocketPath(), err)
	} else if !daemonMayStart(err) {
		return fmt.Errorf("daemon state unknown after connection failure; not starting: %w", err)
	} else if restrictedSessionClient(os.Getenv) {
		return fmt.Errorf("refusing to start a daemon from inside an amux agent: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	_ = os.MkdirAll(core.StateDir(), 0o755)
	_ = os.WriteFile(core.PidPath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
	defer os.Remove(core.PidPath())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	daemon.DumpGoroutinesOnSignal(ctx, os.Stderr)
	// Bound daemon.log by size. Only acts when stdout/stderr are that file (the
	// spawned daemon); a daemon run in a terminal is left alone.
	logrotate.Watch(ctx, logrotate.Policy{
		Path: core.LogPath(), MaxBytes: logrotate.DefaultMaxBytes, Keep: logrotate.DefaultKeep,
		FDs: []int{1, 2}, MaxTotal: logrotate.DefaultMaxTotal, Compress: true,
	}, logrotate.DefaultInterval, log.Printf)
	d := daemon.Default(self)
	if err := d.Run(ctx); err != nil || !d.RestartForUpdate() || ctx.Err() != nil {
		return err
	}
	// The daemon stopped itself, with no agents hosted, to load an update that
	// is now installed at self. Teardown released the socket and singleton
	// lock; re-executing in place keeps the PID, detached session and log fds.
	stop()
	_ = os.Remove(core.PidPath())
	log.Printf("self-update: re-executing %s", self)
	return syscall.Exec(self, []string{self, "daemon"}, os.Environ())
}

// daemonStop turns the running daemon down through the authenticated host
// control stream. A pidfile is diagnostic only: stale integer process IDs are
// never signalled. With mustExist=false a not-running daemon is not an error
// (used by restart, which then just starts a fresh one).
func daemonStop(mustExist bool) error {
	c, err := startupDial()
	if err != nil {
		if daemonMayStart(err) {
			if mustExist {
				return fmt.Errorf("daemon is not running: %w", err)
			}
			return nil
		}
		return fmt.Errorf("cannot authenticate daemon shutdown: %w", daemonConnectionError(err))
	}
	if err := c.Shutdown(); err != nil {
		_ = c.Close()
		return fmt.Errorf("daemon rejected shutdown: %w", err)
	}
	_ = c.Close()
	return waitDaemonStopped()
}

// waitDaemonStopped waits for both endpoint removal and singleton-lock
// release. Socket absence alone is not process identity and can precede final
// engine cleanup.
//
// While it waits it shows what the daemon's teardown is waiting on (busy
// agents finishing their turn, SIGTERM/SIGKILL escalation, lock release), read
// from the status file the daemon publishes; the shutdown connection itself is
// drained before any agent stops, so it cannot carry this.
func waitDaemonStopped() error {
	start := time.Now()
	deadline := start.Add(daemonStopBudget())
	progress := newLifecycleProgress("daemon stopping", daemonstatus.OpStopping, daemonStopBudget())
	defer progress.finish()
	var lastErr error
	for time.Now().Before(deadline) {
		if probeErr := startupProbe(); probeErr == nil {
			lastErr = fmt.Errorf("daemon still accepting authenticated connections")
		} else if !daemonMayStart(probeErr) {
			return fmt.Errorf("cannot verify daemon shutdown: %w", daemonConnectionError(probeErr))
		} else if singletonReleased() {
			progress.finish()
			fmt.Printf("daemon stopped (%s)\n", time.Since(start).Round(100*time.Millisecond))
			return nil
		} else {
			lastErr = fmt.Errorf("daemon listener closed but singleton lock is still held (%s)", daemon.SingletonHolder())
		}
		progress.update(time.Now())
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not complete authenticated shutdown within %s: %w", daemonStopBudget().Round(time.Second), lastErr)
}

func singletonReleased() bool {
	f, err := os.OpenFile(core.DaemonLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}

// daemonStart turns a daemon up if one isn't already answering, reusing the same
// detached-spawn path bare `amux` uses.
func daemonStart(self string) error {
	c, initialErr := startupDial()
	if initialErr == nil {
		_ = c.Close()
		fmt.Println("daemon already running")
		return nil
	}
	if err := startDaemonAfterDial(self, initialErr, true); err != nil {
		return err
	}
	fmt.Println("daemon started")
	return nil
}

// daemonRestart turns the daemon down and back up so it loads a freshly installed
// binary. The daemon owns its agents' processes, so a restart stops them — this
// is the deliberate, explicit way to do that.
func daemonRestart() error {
	// A session client must never stop the host daemon and then discover that its
	// sandbox cannot start the replacement. The operator lifecycle stays outside
	// agent sessions; agents only connect to the daemon they were launched under.
	if restrictedSessionClient(os.Getenv) {
		return fmt.Errorf("refusing to restart the daemon from inside an amux agent")
	}
	// Only an absent or stale listener may continue into startup. Every live
	// daemon is stopped through its authenticated control stream; pidfiles are
	// never process authority.
	// Permission, authentication, and unexpected transport failures leave daemon
	// state unknown and must not turn restart into a destructive guess. An integer
	// PID is diagnostic only and is never process authority.
	if c, err := startupDial(); err == nil {
		_ = c.Close()
	} else if daemonAccessDenied(err) {
		return fmt.Errorf("daemon state unknown: access denied at %s; not restarting: %w", core.SocketPath(), err)
	} else if !daemonMayStart(err) {
		return fmt.Errorf("daemon state unknown after connection failure; not restarting: %w", err)
	}
	// Reject malformed rollout config before stopping a healthy daemon.
	if _, err := amuxcfg.ResolveCodexControl(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := daemonStop(false); err != nil {
		return err
	}
	// Ensure the socket is free before starting; daemonStop already waited on the
	// process, but give the socket file a beat to disappear on slow filesystems.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := startupDial()
		if err != nil {
			if !daemonMayStart(err) {
				return fmt.Errorf("cannot verify daemon stopped: %w", daemonConnectionError(err))
			}
			break
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	return daemonStart(self)
}

// daemonPid reads the daemon pidfile.
func daemonPid() (int, error) {
	b, err := os.ReadFile(core.PidPath())
	if err != nil {
		return 0, fmt.Errorf("no daemon pidfile (%s): %w", core.PidPath(), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("bad pid in %s: %w", core.PidPath(), err)
	}
	return pid, nil
}

// processAlive reports whether a process exists (signal 0 probes without
// delivering a signal).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// daemonAccessDenied recognizes permission failures through net.OpError and
// os.PathError wrappers. They leave daemon existence and liveness unknown.
func daemonAccessDenied(err error) bool {
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

// daemonMayStart is deliberately a positive allowlist. ENOENT means no socket
// path and ECONNREFUSED means no listener owns the path; every other error may
// represent a live daemon, an authorization rejection, or an unhealthy
// transport and must not cause a competing process to start.
func daemonMayStart(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// restrictedSessionClient identifies a CLI launched inside an amux agent. Both
// variables name the same store session; AMUX_WORKSPACE is the legacy alias.
func restrictedSessionClient(getenv func(string) string) bool {
	return sessionContextRestricted() ||
		strings.TrimSpace(getenv("AMUX_WORKGROUP")) != "" ||
		strings.TrimSpace(getenv("AMUX_WORKSPACE")) != ""
}

func restrictedBySessionContext(load func() (access.SessionContext, error)) bool {
	_, err := load()
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

// daemonConnectionError renders a failed client dial without guessing about
// daemon liveness. Only the same absent/stale-listener errors allowed to trigger
// startup are called offline.
func daemonConnectionError(err error) error {
	if daemonMayStart(err) {
		return fmt.Errorf("daemon offline: %w", err)
	}
	if daemonAccessDenied(err) {
		return fmt.Errorf("daemon state unknown; access denied at %s: %w", core.SocketPath(), err)
	}
	return fmt.Errorf("daemon state unknown; connection failed at %s: %w", core.SocketPath(), err)
}

// ensureDaemon starts a detached daemon if the socket isn't already answering,
// then waits briefly for it to come up.
func ensureDaemon(self string) error {
	c, initialErr := startupDial()
	if initialErr == nil {
		_ = c.Close()
		return nil
	}
	return startDaemonAfterDial(self, initialErr, false)
}

// startDaemonAfterDial applies the startup policy to the exact error from the
// caller's first connection attempt. Keeping that error avoids a second probe
// racing to a different conclusion and makes every later failure inspectable.
func startDaemonAfterDial(self string, initialErr error, explicitBootstrap bool) error {
	if daemonAccessDenied(initialErr) {
		return fmt.Errorf("daemon state unknown: access denied at %s; not starting: %w", core.SocketPath(), initialErr)
	}
	if !daemonMayStart(initialErr) {
		return fmt.Errorf("daemon state unknown after connection failure; not starting: %w", initialErr)
	}
	if restrictedSessionClient(os.Getenv) {
		return fmt.Errorf("refusing to start a daemon from inside an amux agent: %w", initialErr)
	}
	if !explicitBootstrap {
		if err := protectedHostStartup(); err != nil {
			return fmt.Errorf("automatic daemon startup requires a protected host credential; run `amux daemon start` explicitly to bootstrap: %w", err)
		}
	}
	// Auto-start and manual start share this validation and child entrypoint.
	if _, err := amuxcfg.ResolveCodexControl(); err != nil {
		return fmt.Errorf("initial daemon connection failed (%w); startup configuration invalid: %w", initialErr, err)
	}
	// No daemon answers, so a staged protocol-changing release can be installed
	// before one starts.
	applyStagedBeforeStart()
	_ = os.MkdirAll(core.StateDir(), 0o755)
	logf, _ := os.OpenFile(core.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logf != nil {
		defer logf.Close()
	}

	cmd := daemonCommand(self, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from our session
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("initial daemon connection failed (%w); start daemon: %w", initialErr, err)
	}
	// Reap the child only to learn whether it is still starting: a daemon
	// that is alive but slow is waited for, one that exited is reported now.
	// The child is in its own session, so this process exiting never stops it.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// Only an explicit start reports progress: auto-start precedes the TUI and
	// other commands whose output must not carry lifecycle chatter.
	var progress *lifecycleProgress
	if explicitBootstrap {
		progress = newLifecycleProgress("daemon starting", daemonstatus.OpStarting, daemonStartupBudget)
	}
	return awaitDaemonReady(initialErr, exited, daemonStartupBudget, progress)
}

// awaitDaemonReady waits for the spawned daemon to accept a connection. A
// fixed few-second window used to report failure for every startup that had
// many mailboxes to reconcile, although the daemon came up moments later.
//
// A non-nil progress shows the daemon's startup phase (mailbox reconciliation)
// while it waits.
func awaitDaemonReady(initialErr error, exited <-chan error, budget time.Duration, progress *lifecycleProgress) error {
	defer progress.finish()
	lastErr := initialErr
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		err := startupProbe()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case waitErr := <-exited:
			// The child returns cleanly when another daemon already owns the
			// socket; that daemon answering is success too.
			if lastErr = startupProbe(); lastErr == nil {
				return nil
			}
			if waitErr == nil {
				waitErr = errors.New("exited")
			}
			if !singletonReleased() {
				// The child lost the singleton race to a daemon that no longer
				// answers (e.g. one wedged mid-shutdown): name it.
				return fmt.Errorf("daemon process stopped before accepting connections (%v): singleton lock %s %s; initial connection failed: %w; last connection failed: %w", waitErr, core.DaemonLockPath(), daemon.SingletonHolder(), initialErr, lastErr)
			}
			return fmt.Errorf("daemon process stopped before accepting connections (%v; see %s); initial connection failed: %w; last connection failed: %w", waitErr, core.LogPath(), initialErr, lastErr)
		case <-time.After(75 * time.Millisecond):
		}
		progress.update(time.Now())
	}
	return fmt.Errorf("daemon did not come up within %s (see %s); initial connection failed: %w; last connection failed: %w", budget, core.LogPath(), initialErr, lastErr)
}

// cmdNative launches the native Bubble Tea TUI (bare `amux`). It ensures the
// daemon is up for state, then runs the TUI; the daemon's engine hosts the
// agents, so quitting the TUI just detaches and the agents keep running.
//
// focusID, when set, is the workgroup or agent the dashboard opens on (what
// `amux workgroup open <id>` passes); empty just opens the dashboard.
func cmdNative(focusID string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cleanupGlobalHooks()
	if err := ensureDaemon(self); err != nil {
		fmt.Fprintln(os.Stderr, "amux: warning: daemon not started:", err)
	}
	return nativetui.Run(focusID)
}

// cmdStatus prints the current snapshot for scripting. With --json it emits the
// raw snapshot (every Session field); otherwise an aligned plain-text table. It
// reads the daemon's canonical state, never the store.
func cmdStatus(args []string) error {
	asJSON := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			asJSON = true
		}
	}
	if sessionContextRestricted() {
		var sessions []core.Session
		if err := queryRows(core.QuerySnapshot, &sessions); err != nil {
			return err
		}
		snapshot := core.Snapshot{Type: "snapshot", Sessions: sessions}
		if asJSON {
			// The overview is present only for a session that may read the queue
			// (the overseer, the admin console); every other caller keeps the
			// exact snapshot shape.
			var out any = snapshot
			if ov := readStatusOverview(); ov != nil {
				out = struct {
					core.Snapshot
					Overview *statusOverview `json:"overview"`
				}{snapshot, ov}
			}
			b, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		if len(sessions) == 0 {
			fmt.Println("(no visible sessions)")
			return nil
		}
		for _, s := range sessions {
			state := s.State
			if state == "" {
				state = core.StateIdle
			}
			fmt.Printf("%-20s %-8s %-8s %s%s\n", s.Title, s.Kind, state, s.Cwd, statusEnrichment(s))
		}
		return nil
	}
	c, err := startupDial()
	if err != nil {
		return daemonConnectionError(err)
	}
	defer c.Close()
	for {
		f, err := c.Next()
		if err != nil {
			return err
		}
		if f.Snapshot == nil {
			continue
		}
		if asJSON {
			// Per-session cpu/memory from the daemon's cached resource sample;
			// omitted (best effort) when the daemon predates it.
			var res *procstat.Snapshot
			if r, err := queryResources(); err == nil {
				res = &r
			}
			out := statusWithResources(*f.Snapshot, res)
			out.Overview = readStatusOverview()
			b, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		// stderr, so the plain table stays parseable.
		defer printPendingUpdate()
		if len(f.Snapshot.Sessions) == 0 {
			fmt.Println("(no workgroups — `amux workgroup new`)")
			return nil
		}
		for _, s := range f.Snapshot.Sessions {
			state := s.State
			if state == "" {
				state = core.StateIdle
			}
			fmt.Printf("%-20s %-8s %-8s %s%s\n", s.Title, s.Kind, state, s.Cwd, statusEnrichment(s))
		}
		return nil
	}
}
