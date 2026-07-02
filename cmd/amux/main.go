// Command amux is an AI-native terminal control plane: it runs agent sessions as
// daemon-owned processes and shows a persistent, interactive dashboard of every
// local and cloud agent.
//
// Subcommands:
//
//	(bare)   open the native dashboard TUI
//	daemon   run the polling/serving daemon (foreground)
//	agent    self-reporting run by an agent about itself (status/hook/name)
//	status   print current sessions as text and exit
//	version  print version
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/nativetui"
	"amux/internal/vtdemo"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		// Bare `amux` opens the native TUI. `amux --help`/-h/help still print
		// usage (those carry an arg, so they fall through to the switch below).
		if err := cmdNative(); err != nil {
			fmt.Fprintln(os.Stderr, "amux:", err)
			os.Exit(1)
		}
		return
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = cmdDaemon()
	case "serve":
		err = cmdServe(args)
	case "harness":
		err = cmdHarness()
	case "provide":
		err = cmdProvide(args)
	case "_vtdemo": // hidden: embedded-terminal fidelity check (Phase 0 spike)
		err = vtdemo.Run(args)
	case "agent":
		err = cmdAgent(args)
	case "hook":
		err = cmdAgentStatus(args) // deprecated alias for "amux agent hook"
	case "status":
		err = cmdStatus(args)
	case "refresh":
		err = cmdRefresh()
	case "doctor", "health", "check":
		err = cmdDoctor()
	case "repo":
		err = cmdRepo(args)
	case "workgroup", "wg", "session", "ses", "workspace", "ws":
		err = cmdSession(args)
	case "name":
		err = cmdName(args)
	case "do":
		err = cmdDo(args)
	case "version", "--version", "-v":
		fmt.Println("amux", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "amux: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amux:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `amux — AI-native terminal control plane

usage: amux <command>

  (bare)             open the workspace dashboard (native TUI)
  repo add <src>     track a repo (clone a git URL, or register a local path)
  repo ls | rm       list / untrack repositories
  workgroup new      create a work-scoped workgroup via a config page, then open
  workgroup repo <r> start a single-repo (repo-scoped) agent on a tracked repo
  workgroup move <a> [<root>|--new]  re-parent an agent into a work-scoped workgroup
  workgroup open <id> open/switch to a workgroup
  workgroup rename <id> <name>  set a workgroup/agent display name (id is unchanged)
  workgroup rm <id>  delete a workgroup (removes its worktrees + branches)
  name <text>        set the current workgroup's display name (for the agent)
  status [--json]    print workgroups and exit (--json for the raw snapshot)
  do <action> ...    drive a daemon action (see "amux do" actions below)
  refresh            ask the daemon to re-poll its sources now
  doctor             health check: dependencies (fzf/claude/gh/…) + runtime
  provide <addr>     dial a remote orchestrator and serve panes (provider mode)
  daemon             run the daemon in the foreground (usually automatic)
  version            print version

amux do <action> drives the daemon's control API from scripts (no direct store
access). Positional [id]/[kind] still work; flags reach the rest:

  --target, -t <root>   destination root id (for "move")
  --kind <kind>         agent kind (for "new")
  --cwd <dir>           working directory (for "new")
  --field, -f key=val   form field, repeatable (add-agent, new-workgroup, …)

  amux do rename <id> -f name="api spike"
  amux do move <id> --target <root>
  amux do add-agent <root> -f repos=api,web -f prompt="port the auth flow"
  amux do new-workgroup -f name=infra -f repos=infra -f prompt="upgrade CI"

Set AMUX_SKIP=1 in your shell to bypass auto-launch.
`)
}

func cmdDaemon() error {
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

// ensureDaemon starts a detached daemon if the socket isn't already answering,
// then waits briefly for it to come up.
func ensureDaemon(self string) error {
	if c, err := daemon.Dial(); err == nil {
		_ = c.Close()
		return nil
	}
	_ = os.MkdirAll(core.StateDir(), 0o755)
	logf, _ := os.OpenFile(core.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)

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
func cmdNative() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cleanupGlobalHooks()
	if err := ensureDaemon(self); err != nil {
		fmt.Fprintln(os.Stderr, "amux: warning: daemon not started:", err)
	}
	return nativetui.Run()
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
			fmt.Println("(no workspaces — `amux workspace new`)")
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
