// Command amux is an AI-native terminal control plane: it runs agent
// sessions inside an isolated tmux server and shows a persistent, interactive
// dashboard of every local and cloud agent.
//
// Subcommands:
//
//	up           ensure the daemon + isolated tmux server, then attach
//	daemon       run the polling/serving daemon (foreground)
//	rail         render the compact side-pane dashboard (used inside tmux)
//	dash         render the full-screen dashboard
//	rail-attach  (tmux hook) add a rail pane to a window
//	status       print current sessions as text and exit
//	init         (re)write the isolated tmux config
//	version      print version
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/tmuxctl"
	"amux/internal/tui"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp()
	case "daemon":
		err = cmdDaemon()
	case "rail":
		err = tui.Run(false)
	case "dash":
		err = tui.Run(true)
	case "rail-attach":
		err = cmdRailAttach(args)
	case "status":
		err = cmdStatus()
	case "repo":
		err = cmdRepo(args)
	case "workspace", "ws":
		err = cmdWorkspace(args)
	case "name":
		err = cmdName(args)
	case "console":
		err = cmdConsole()
	case "do":
		err = cmdDo(args)
	case "init":
		err = ensureConf(true)
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

  up                 start everything and attach to the isolated tmux server
  dash               open the full-screen workspace dashboard
  repo add <src>     track a repo (clone a git URL, or register a local path)
  repo ls | rm       list / untrack repositories
  workspace new      create a workspace via a config page, then open it
  workspace open <id> open/switch to a workspace
  workspace rm <id>  delete a workspace (removes its worktrees + branches)
  name <text>        set the current workspace's display name (for the agent)
  status             print workspaces and exit
  init               (re)write the isolated tmux config
  daemon             run the daemon in the foreground (usually automatic)
  version            print version

Set AMUX_SKIP=1 in your shell to bypass auto-launch.
`)
}

// cmdUp ensures the daemon and isolated tmux server are running, then replaces
// this process with a tmux client attached to the "main" session.
func cmdUp() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := ensureConf(false); err != nil {
		return err
	}
	if err := ensureDaemon(self); err != nil {
		// Non-fatal: the dashboard will show "offline" and reconnect.
		fmt.Fprintln(os.Stderr, "amux: warning: daemon not started:", err)
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found on PATH: %w", err)
	}
	// attach-or-create the single session; -f points at the isolated config.
	argv := []string{
		"tmux", "-L", core.TmuxSocket, "-f", core.TmuxConfPath(),
		"new-session", "-A", "-s", core.SessionName,
	}
	return syscall.Exec(tmuxBin, argv, os.Environ())
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

// cmdRailAttach is invoked by the tmux after-new-window/after-new-session hook.
func cmdRailAttach(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("rail-attach requires a window id")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tmuxctl.AttachRail(ctx, args[0], self, "rail")
}

// cmdDo sends a single control action to the daemon and prints the result.
// Usage: amux do <attach|kill|resume|new|refresh> [id] [kind]
func cmdDo(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: amux do <action> [id] [kind]")
	}
	a := core.Action{Action: args[0]}
	if len(args) >= 2 {
		a.ID = args[1]
	}
	if len(args) >= 3 {
		a.Kind = args[2]
	}
	c, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("daemon offline: %w", err)
	}
	defer c.Close()
	if err := c.Send(a); err != nil {
		return err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return err
		}
		if f.Result != nil {
			if !f.Result.OK {
				return fmt.Errorf("%s", f.Result.Error)
			}
			fmt.Println("ok")
			return nil
		}
	}
}

// cmdStatus prints the current snapshot as plain text (no TUI) for scripting.
func cmdStatus() error {
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
		if len(f.Snapshot.Sessions) == 0 {
			fmt.Println("(no workspaces — `amux workspace new`)")
			return nil
		}
		for _, s := range f.Snapshot.Sessions {
			state := "idle"
			if s.WindowID != "" {
				state = "running"
			}
			fmt.Printf("%-20s %-8s %-8s %s\n", s.Title, s.Kind, state, s.Cwd)
		}
		return nil
	}
}
