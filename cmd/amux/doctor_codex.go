package main

import (
	"fmt"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/daemon"
)

func controlOverride(c amuxcfg.Control) string {
	if !c.OverrideSet {
		return "unset"
	}
	return fmt.Sprintf("%q", c.Override)
}

func storedControl(value string) string {
	if value == "" {
		return "unset (default pty)"
	}
	return value
}

// reportCodexControl never starts a daemon. Its query works on Linux and macOS;
// an older or unreachable daemon has unknown selection, regardless of this shell.
func reportCodexControl() {
	fmt.Println("\nCodex control")
	current, err := amuxcfg.ResolveCodexControl()
	fmt.Printf("  config file: %s\n", core.ConfigPath())
	if err != nil {
		fmt.Printf("  ✗ saved setting: %v (startup rejected; fix before restarting)\n", err)
	} else {
		fmt.Printf("  saved codex.control: %s\n", storedControl(current.Persisted))
		fmt.Printf("  next start from this shell: %s (source=%s)\n", current.Effective, current.Source)
	}
	fmt.Printf("  shell %s: %s (not daemon state)\n", amuxcfg.ControlEnv, controlOverride(current))
	if current.Warning != "" {
		fmt.Printf("  ⚠ %s\n", current.Warning)
	}
	c, dialErr := daemon.Dial()
	if dialErr != nil {
		fmt.Printf("  running daemon: unknown/offline (%v)\n", dialErr)
		return
	}
	defer c.Close()
	running, queryErr := c.CodexControl()
	if queryErr != nil {
		fmt.Printf("  running daemon: selection unknown (diagnostic query unavailable: %v); older daemons require an update and restart to report it\n", queryErr)
		return
	}
	fmt.Printf("  running daemon: %s (source=%s; reported over %s)\n", running.Effective, running.Source, core.SocketPath())
	fmt.Printf("    at startup: config=%s; saved=%s; %s=%s\n", running.ConfigPath, storedControl(running.Persisted), amuxcfg.ControlEnv, controlOverride(running))
	if running.Warning != "" {
		fmt.Printf("  ⚠ daemon: %s\n", running.Warning)
	}
	if err == nil && (current.Persisted != running.Persisted || current.ConfigPath != running.ConfigPath || current.Effective != running.Effective) {
		fmt.Println("  ⚠ startup inputs differ; amux daemon restart is needed to apply changes (stops hosted agents); review the override first")
	}
}
