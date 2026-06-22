// Package tmuxctl is a thin wrapper over `tmux -L amux ...`. Every call is
// scoped to the isolated server's socket, so nothing here can touch the user's
// default tmux server.
package tmuxctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"amux/internal/core"
)

// Run executes a tmux command against the isolated server and returns trimmed
// stdout.
func Run(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"-L", core.TmuxSocket}, args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// ServerRunning reports whether the isolated tmux server is up.
func ServerRunning(ctx context.Context) bool {
	_, err := Run(ctx, "list-sessions")
	return err == nil
}

// PaneWindows returns a map of pane_pid -> window_id for every pane in the
// isolated server. A missing server is treated as "no panes", not an error.
func PaneWindows(ctx context.Context) map[int]string {
	out, err := Run(ctx, "list-panes", "-a", "-F", "#{pane_pid} #{window_id}")
	m := map[int]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if pid, err := strconv.Atoi(fields[0]); err == nil {
			m[pid] = fields[1]
		}
	}
	return m
}

// CurrentWindow returns the window id of the pane this process is running in,
// resolved from $TMUX_PANE, or "" if it isn't inside the isolated server. The
// rail uses this to know which agent's window it belongs to.
func CurrentWindow(ctx context.Context) string {
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	out, err := Run(ctx, "display-message", "-p", "-t", pane, "#{window_id}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// SwitchClient jumps the attached client to the given window/target.
func SwitchClient(ctx context.Context, target string) error {
	_, err := Run(ctx, "switch-client", "-t", target)
	return err
}

// WorkspaceWindows maps workspace name -> window id for windows tagged with the
// @amx_ws window option (i.e. the open workspaces).
func WorkspaceWindows(ctx context.Context) map[string]string {
	out, err := Run(ctx, "list-windows", "-a", "-F", "#{@amx_ws}|#{window_id}")
	m := map[string]string{}
	if err != nil {
		return m
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && parts[0] != "" {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

// TagWorkspace marks a window as belonging to a workspace.
func TagWorkspace(ctx context.Context, window, name string) error {
	_, err := Run(ctx, "set-option", "-w", "-t", window, "@amx_ws", name)
	return err
}

// NewWindow opens a new window running cmd in cwd, with the given environment
// (KEY=VALUE strings) set for the window, and returns its window id.
func NewWindow(ctx context.Context, cwd, name string, env []string, cmd ...string) (string, error) {
	args := []string{"new-window", "-P", "-F", "#{window_id}"}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	if name != "" {
		args = append(args, "-n", name)
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, "--")
	args = append(args, cmd...)
	return Run(ctx, args...)
}

// KillWindow kills a window by target (window id).
func KillWindow(ctx context.Context, target string) error {
	_, err := Run(ctx, "kill-window", "-t", target)
	return err
}

// RailPresent reports whether the given window already carries a rail pane
// (marked with the user option @amx=rail), so we never double-attach one.
func RailPresent(ctx context.Context, window string) bool {
	out, err := Run(ctx, "list-panes", "-t", window, "-F", "#{@amx}")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "rail" {
			return true
		}
	}
	return false
}

// AttachRail splits a narrow, inactive rail pane onto the left of window and
// marks it so it's recognizable and won't be duplicated. railCmd is the full
// argv of the rail process (absolute binary path + "rail").
func AttachRail(ctx context.Context, window string, railCmd ...string) error {
	if RailPresent(ctx, window) {
		return nil
	}
	args := []string{
		"split-window", "-hb", "-d",
		"-l", strconv.Itoa(core.RailWidth),
		"-t", window,
		"-P", "-F", "#{pane_id}",
		"--",
	}
	args = append(args, railCmd...)
	pane, err := Run(ctx, args...)
	if err != nil {
		return err
	}
	// Mark the pane so RailPresent recognizes it on the next window.
	_, _ = Run(ctx, "set-option", "-p", "-t", pane, "@amx", "rail")
	// Deterministically focus the work pane (the agent), so you land in it — not
	// the rail — regardless of how split-window left the active pane.
	focusWorkPane(ctx, window)
	return nil
}

// ReloadRails kills every rail pane in the isolated server and re-attaches a
// fresh one (running railCmd) to each window — so a newly installed binary's UI
// takes effect without restarting the agent panes. A missing server is a no-op.
func ReloadRails(ctx context.Context, railCmd ...string) error {
	out, err := Run(ctx, "list-panes", "-a", "-F", "#{@amx}|#{pane_id}")
	if err != nil {
		return nil // server not up: nothing to reload
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && parts[0] == "rail" && parts[1] != "" {
			_, _ = Run(ctx, "kill-pane", "-t", parts[1])
		}
	}
	windows, err := Run(ctx, "list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return err
	}
	for _, w := range strings.Split(windows, "\n") {
		if w = strings.TrimSpace(w); w != "" {
			if err := AttachRail(ctx, w, railCmd...); err != nil {
				return err
			}
		}
	}
	return nil
}

// focusWorkPane selects the first non-rail pane in window so focus is on the
// agent/shell, not the dashboard rail.
func focusWorkPane(ctx context.Context, window string) {
	if p := WorkPane(ctx, window); p != "" {
		_, _ = Run(ctx, "select-pane", "-t", p)
	}
}

// WorkPane returns the id of window's first non-rail pane (the agent/shell), or
// "" if none can be found.
func WorkPane(ctx context.Context, window string) string {
	out, err := Run(ctx, "list-panes", "-t", window, "-F", "#{@amx}|#{pane_id}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) == 2 && parts[0] != "rail" && parts[1] != "" {
			return parts[1]
		}
	}
	return ""
}
