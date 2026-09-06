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
//	version  print CLI, daemon, and database versions
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/nativetui"
	"amux/internal/vtdemo"
)

func main() {
	if len(os.Args) < 2 {
		// Bare `amux` opens the native TUI. `amux --help`/-h/help still print
		// usage (those carry an arg, so they fall through to the switch below).
		if err := cmdNative(""); err != nil {
			fmt.Fprintln(os.Stderr, "amux:", err)
			os.Exit(1)
		}
		return
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	c := lookupCommand(cmd)
	if c == nil {
		fmt.Fprintf(os.Stderr, "amux: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err := c.run(args); err != nil {
		fmt.Fprintln(os.Stderr, "amux:", err)
		os.Exit(1)
	}
}

// command is one top-level verb of the CLI: the word (plus any aliases) the user
// types and the function it runs.
type command struct {
	names  []string // primary name first, then aliases
	run    func(args []string) error
	hidden bool // internal/diagnostic surface, deliberately absent from usage()
}

// commands is the whole top-level dispatch. It's a table rather than a switch so
// the dispatch and the help text can be checked against each other: the contract
// test walks usage() and this table in both directions, so a command that is
// advertised but not dispatched (as `workgroup open` was) fails the tests instead
// of the user's shell.
var commands = []command{
	{names: []string{"daemon"}, run: cmdDaemon},
	{names: []string{"serve"}, run: cmdServe, hidden: true},
	{names: []string{"harness"}, run: func([]string) error { return cmdHarness() }, hidden: true},
	{names: []string{"provide"}, run: cmdProvide},
	// hidden: embedded-terminal fidelity check (Phase 0 spike)
	{names: []string{"_vtdemo"}, run: vtdemo.Run, hidden: true},
	{names: []string{"agent"}, run: cmdAgent},
	// deprecated alias for "amux agent hook"
	{names: []string{"hook"}, run: cmdAgentStatus, hidden: true},
	{names: []string{"status"}, run: cmdStatus},
	{names: []string{"refresh"}, run: func([]string) error { return cmdRefresh() }},
	{names: []string{"doctor", "health", "check"}, run: func([]string) error { return cmdDoctor() }},
	{names: []string{"config", "conf", "cfg"}, run: cmdConfig},
	{names: []string{"sandbox", "sb"}, run: cmdSandbox},
	{names: []string{"repo"}, run: cmdRepo},
	{names: []string{"workgroup", "wg", "session", "ses", "workspace", "ws"}, run: cmdSession},
	// deprecated top-level alias for "amux agent name"
	{names: []string{"name"}, run: cmdName, hidden: true},
	{names: []string{"do"}, run: cmdDo},
	{names: []string{"version", "--version", "-v"}, run: func([]string) error { return cmdVersion() }},
	// hidden: help is what prints usage(), so it doesn't list itself
	{names: []string{"help", "-h", "--help"}, run: func([]string) error { usage(); return nil }, hidden: true},
}

// lookupCommand resolves a command word (or one of its aliases) to its entry,
// nil when amux has no such command.
func lookupCommand(name string) *command {
	for i := range commands {
		for _, n := range commands[i].names {
			if n == name {
				return &commands[i]
			}
		}
	}
	return nil
}

// usage prints the top-level help. The command block below is parsed by the
// dispatch contract test, which needs each entry to be "  <spec>  <description>"
// — two or more spaces between the invocation and its prose — so it can tell a
// subcommand word from the description that follows it.
func usage() {
	fmt.Fprint(os.Stderr, `amux — AI-native terminal control plane

usage: amux <command>

  (bare)             open the workgroup dashboard (native TUI)
  repo add <src>     track a repo (clone a git URL, or register a local path)
  repo ls | rm       list / untrack repositories
  workgroup new      create a work-scoped workgroup via a config page, then open
  workgroup repo <r>  start a single-repo (repo-scoped) agent on a tracked repo
  workgroup move <a> [<root>|--new]  re-parent an agent into a work-scoped workgroup
  workgroup open <id>  open the dashboard focused on a workgroup or agent
  workgroup rename <id> <name>  set a workgroup/agent display name (id is unchanged)
  workgroup archive | unarchive <id>  mark a workgroup done / bring it back
  workgroup rm <id>  delete a workgroup (removes its worktrees + branches)
  workgroup ls       list workgroups and their agents
  agent name <text>  set the calling agent's display name (from its own tab)
  status [--json]    print workgroups and exit (--json for the raw snapshot)
  do <action> ...    drive a daemon action (see "amux do" actions below)
  refresh            ask the daemon to re-poll its sources now
  doctor             health check: versions, compatibility, dependencies + runtime
  config [ls|get|set|unset|path]  show or change amux settings (keybindings, Codex control)
  sandbox drift [<id>]  list edits agents made to their private copy of your harness config
  sandbox promote | reset <id> <path>  propagate an agent's config edit to yours / discard it
  provide [<addr>]   dial a remote orchestrator and serve panes (provider mode)
  provide install | uninstall  run provider mode as a user service (survives reboot)
  daemon [stop|start|restart]  run/control the daemon (restart loads a new binary)
  version            print CLI, daemon and database versions + compatibility

amux do <action> drives the daemon's control API from scripts (no direct store
access). Positional [id]/[kind] still work; flags reach the rest:

  --target, -t <root>   destination workgroup id (for "move")
  --field, -f key=val   form field, repeatable (add-agent, new-workgroup, …)

An unknown action prints the full list of valid ones.

  amux do rename <id> -f name="api spike"
  amux do move <id> --target <root>
  amux do add-agent <root> -f repos=api,web -f prompt="port the auth flow"
  amux do new-workgroup -f name=infra -f repos=infra -f prompt="upgrade CI"

Set AMUX_SKIP=1 in your shell to bypass auto-launch.
`)
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

// cmdDaemon dispatches the daemon lifecycle.
func cmdDaemon(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if isHelpFlag(sub) {
		daemonUsage()
		return nil
	}
	run, ok := daemonSubcommands[sub]
	if !ok {
		daemonUsage()
		return fmt.Errorf("unknown daemon subcommand %q (want stop|start|restart)", sub)
	}
	return run()
}

func daemonUsage() {
	fmt.Fprint(os.Stderr, `amux daemon — run and control the local daemon

The daemon owns the store and the agent engine: it hosts every running agent, so
stopping or restarting it stops the agents it hosts. Bare `+"`amux daemon`"+` runs one
in the foreground (what auto-launch spawns); the verbs below control a detached
one.

usage: amux daemon [command]

  (bare)             run the daemon in the foreground until signalled
  stop               stop the running daemon cleanly  (alias: down)
  start              start a detached daemon if none is answering  (alias: up)
  restart            stop, then start — how a newly installed binary is loaded
                     (alias: reload)
`)
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
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		return nil
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
	return daemon.Default(self).Run(ctx)
}

// daemonStop turns the running daemon down. It signals the pidfile's process
// with SIGTERM so the daemon shuts its engine down cleanly (stopping the agents
// it hosts), then waits for the socket to go quiet, escalating to SIGKILL if the
// daemon doesn't exit in time. With mustExist=false a not-running daemon is not
// an error (used by restart, which then just starts a fresh one).
func daemonStop(mustExist bool) error {
	pid, err := daemonPid()
	if err != nil {
		if !mustExist {
			// No pidfile, but the socket might still answer a stale daemon.
			if c, derr := daemon.Dial(); derr == nil {
				_ = c.Close()
				return fmt.Errorf("daemon is answering %s but its pidfile is missing; stop it by hand", core.SocketPath())
			}
			return nil // nothing to stop
		}
		return err
	}
	if !processAlive(pid) {
		_ = os.Remove(core.PidPath())
		if mustExist {
			fmt.Printf("daemon (pid %d) was not running; cleared stale pidfile\n", pid)
		}
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon (pid %d): %w", pid, err)
	}
	// Wait for a clean exit: the daemon stops its agents, removes the socket, and
	// exits. Poll until the process is gone (up to ~12s to allow agent teardown).
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			fmt.Printf("daemon stopped (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Didn't exit gracefully — force it so a restart can proceed.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	fmt.Printf("daemon (pid %d) did not exit in time; sent SIGKILL\n", pid)
	return nil
}

// daemonStart turns a daemon up if one isn't already answering, reusing the same
// detached-spawn path bare `amux` uses.
func daemonStart(self string) error {
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		fmt.Println("daemon already running")
		return nil
	}
	if err := ensureDaemon(self); err != nil {
		return err
	}
	fmt.Println("daemon started")
	return nil
}

// daemonRestart turns the daemon down and back up so it loads a freshly installed
// binary. The daemon owns its agents' processes, so a restart stops them — this
// is the deliberate, explicit way to do that.
func daemonRestart() error {
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
		if _, err := daemon.Dial(); err != nil {
			break
		}
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

// ensureDaemon starts a detached daemon if the socket isn't already answering,
// then waits briefly for it to come up.
func ensureDaemon(self string) error {
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		return nil
	}
	// Auto-start and manual start share this validation and child entrypoint.
	if _, err := amuxcfg.ResolveCodexControl(); err != nil {
		return err
	}
	_ = os.MkdirAll(core.StateDir(), 0o755)
	logf, _ := os.OpenFile(core.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if logf != nil {
		defer logf.Close()
	}

	cmd := exec.Command(self, "daemon")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from our session
	if logf != nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := daemon.Dial(); err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(75 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not come up within timeout (see %s)", core.LogPath())
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
	c, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("daemon offline: %w", err)
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
			b, err := json.MarshalIndent(f.Snapshot, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(b))
			return nil
		}
		if len(f.Snapshot.Sessions) == 0 {
			fmt.Println("(no workgroups — `amux workgroup new`)")
			return nil
		}
		for _, s := range f.Snapshot.Sessions {
			state := s.State
			if state == "" {
				state = core.StateIdle
			}
			fmt.Printf("%-20s %-8s %-8s %s\n", s.Title, s.Kind, state, s.Cwd)
		}
		return nil
	}
}
