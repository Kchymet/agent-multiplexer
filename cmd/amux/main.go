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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/nativetui"
	"amux/internal/tmuxctl"
	"amux/internal/tui"
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
	case "up":
		err = cmdUp()
	case "daemon":
		err = cmdDaemon()
	case "serve":
		err = cmdServe(args)
	case "harness":
		err = cmdHarness()
	case "rail":
		err = tui.Run(false)
	case "dash":
		err = tui.Run(true)
	case "rail-attach":
		err = cmdRailAttach(args)
	case "reload":
		err = cmdReload()
	case "_vtdemo": // hidden: embedded-terminal fidelity check (Phase 0 spike)
		err = vtdemo.Run(args)
	case "hook":
		err = cmdHook(args)
	case "status":
		err = cmdStatus()
	case "doctor", "health", "check":
		err = cmdDoctor()
	case "repo":
		err = cmdRepo(args)
	case "workgroup", "wg", "session", "ses", "workspace", "ws":
		err = cmdSession(args)
	case "name":
		err = cmdName(args)
	case "console":
		err = cmdConsole()
	case "do":
		err = cmdDo(args)
	case "init":
		if err = ensureConf(true); err == nil {
			ensureHooks(true)
		}
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
  workgroup new      create a work-scoped workgroup via a config page, then open
  workgroup repo <r> start a single-repo (repo-scoped) agent on a tracked repo
  workgroup move <a> [<root>|--new]  re-parent an agent into a work-scoped workgroup
  workgroup open <id> open/switch to a workgroup
  workgroup rename <id> <name>  set a workgroup/agent display name (id is unchanged)
  workgroup rm <id>  delete a workgroup (removes its worktrees + branches)
  name <text>        set the current workgroup's display name (for the agent)
  status             print workgroups and exit
  reload             restart the daemon + reload the rails (after an install)
  doctor             health check: dependencies (fzf/claude/gh/…) + runtime
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
	ensureHooks(false) // one-time: install Claude status hooks during first setup
	if err := ensureDaemon(self); err != nil {
		// Non-fatal: the dashboard will show "offline" and reconnect.
		fmt.Fprintln(os.Stderr, "amux: warning: daemon not started:", err)
	}
	setupRails(self) // rails belong to the classic `amux up` entrypoint only
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

// setupRails attaches the side-pane rail to the classic `amux up` session and
// installs a session-scoped hook so windows opened during that session keep
// getting rails — without the old global "every window" hook, so the native
// TUI (and any other tmux usage) leaves windows rail-free. Best-effort.
func setupRails(self string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unsetGlobalRailHooks(ctx) // migrate servers that still carry the old global hooks
	// Ensure `main` exists (detached) so we can target it before the attach.
	_, _ = tmuxctl.Run(ctx, "new-session", "-A", "-d", "-s", core.SessionName)
	// Rail new windows created during this session.
	_, _ = tmuxctl.Run(ctx, "set-hook", "-t", core.SessionName, "after-new-window",
		fmt.Sprintf("run-shell \"%s rail-attach #{window_id}\"", self))
	// Rail the windows that already exist.
	out, err := tmuxctl.Run(ctx, "list-windows", "-t", core.SessionName, "-F", "#{window_id}")
	if err != nil {
		return
	}
	for _, w := range strings.Split(out, "\n") {
		if w = strings.TrimSpace(w); w != "" {
			_ = tmuxctl.AttachRail(ctx, w, self, "rail")
		}
	}
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

// cmdNative launches the native Bubble Tea TUI (bare `amux`). It ensures the
// daemon is up for state, then runs the TUI; tmux still hosts the agents, so
// quitting the TUI just detaches and the agents keep running.
func cmdNative() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := ensureConf(false); err != nil {
		return err
	}
	ensureHooks(false)
	if err := ensureDaemon(self); err != nil {
		fmt.Fprintln(os.Stderr, "amux: warning: daemon not started:", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	unsetGlobalRailHooks(ctx) // native TUI is the sidebar; don't rail windows
	cancel()
	return nativetui.Run()
}

// unsetGlobalRailHooks removes the legacy global rail hooks from a running
// server, so rails are no longer added to every new window. Best-effort.
func unsetGlobalRailHooks(ctx context.Context) {
	_, _ = tmuxctl.Run(ctx, "set-hook", "-gu", "after-new-window")
	_, _ = tmuxctl.Run(ctx, "set-hook", "-gu", "after-new-session")
}

// cmdReload restarts the daemon and re-attaches every rail with the current
// binary, so a freshly installed amux takes effect without disturbing the agent
// panes. (`amux up` only re-attaches to the existing session, leaving the old
// daemon and rail processes running the previous binary.)
func cmdReload() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stopDaemon()
	if err := ensureDaemon(self); err != nil {
		return err
	}
	if err := tmuxctl.ReloadRails(ctx, self, "rail"); err != nil {
		return err
	}
	fmt.Println("reloaded: daemon restarted, rails re-attached")
	return nil
}

// stopDaemon signals the running daemon (if any) to exit and waits briefly for
// it to release its socket, so ensureDaemon then starts a fresh one.
func stopDaemon() {
	b, err := os.ReadFile(core.PidPath())
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
	for i := 0; i < 60; i++ {
		c, err := daemon.Dial()
		if err != nil {
			return // no longer answering — it's down
		}
		_ = c.Close()
		time.Sleep(50 * time.Millisecond)
	}
}

// cmdHook is invoked by Claude Code's status hooks ("amux hook <state>", wired
// up by claudecfg.InstallHooks). It reads the hook payload from stdin to learn
// the Claude session id, then records the activity state for the daemon's poll
// loop to surface in the rail. It must never disrupt the agent, so it swallows
// all errors and exits 0.
func cmdHook(args []string) error {
	if len(args) < 1 {
		return nil
	}
	state := args[0]
	var payload struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}
	if b, err := io.ReadAll(os.Stdin); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &payload)
	}
	_ = core.WriteHookState(payload.SessionID, state, payload.Cwd)
	return nil
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
			state := s.State
			if state == "" {
				state = core.StateIdle
			}
			fmt.Printf("%-20s %-8s %-8s %s\n", s.Title, s.Kind, state, s.Cwd)
		}
		return nil
	}
}
