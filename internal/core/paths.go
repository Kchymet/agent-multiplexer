package core

import (
	"os"
	"path/filepath"
)

// StateDir is where amux keeps runtime state (pidfile, logs, fallback socket).
func StateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "amux")
}

// runtimeDir prefers $XDG_RUNTIME_DIR (tmpfs, per-user) for the control socket,
// falling back to the state dir on systems that don't set it (e.g. macOS).
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return StateDir()
}

// SocketPath is the unix socket the daemon listens on and clients dial.
// Override with $AMUX_SOCK (useful for tests / multiple instances).
func SocketPath() string {
	if p := os.Getenv("AMUX_SOCK"); p != "" {
		return p
	}
	return filepath.Join(runtimeDir(), "amux.sock")
}

// MuxSocketPath is the unix socket the multiplexer server (`amux serve`) listens
// on locally. Override with $AMUX_MUX_SOCK. Remote servers are reached over TCP
// via $AMUX_SERVER instead.
func MuxSocketPath() string {
	if p := os.Getenv("AMUX_MUX_SOCK"); p != "" {
		return p
	}
	return filepath.Join(runtimeDir(), "amux-mux.sock")
}

// DataDir is where amux keeps durable data: the repo store, workspace
// worktrees, and the registry. Honors $XDG_DATA_HOME.
func DataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "amux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "amux")
}

// ReposDir holds the local bare clones used as worktree sources.
func ReposDir() string { return filepath.Join(DataDir(), "repos") }

// WorkspacesDir holds legacy per-workspace directories (pre-hierarchy).
func WorkspacesDir() string { return filepath.Join(DataDir(), "workspaces") }

// SessionsDir holds per-root-session directories of sub-session worktrees.
func SessionsDir() string { return filepath.Join(DataDir(), "sessions") }

// DBPath is the SQLite session store.
func DBPath() string { return filepath.Join(DataDir(), "amux.db") }

// ConsoleDir is the neutral root for the amux control console agent (no repo
// code lives here — just the console's CLAUDE.md).
func ConsoleDir() string { return filepath.Join(DataDir(), "console") }

// RegistryPath is the JSON file tracking repos and workspaces.
func RegistryPath() string { return filepath.Join(DataDir(), "registry.json") }

// PidPath is the daemon pidfile.
func PidPath() string {
	return filepath.Join(StateDir(), "daemon.pid")
}

// LogPath is the daemon log file.
func LogPath() string {
	return filepath.Join(StateDir(), "daemon.log")
}

// LiveAgentsPath is the JSON file recording which engine instances were running,
// so a daemon restart can relaunch them without a UI trigger.
func LiveAgentsPath() string {
	return filepath.Join(StateDir(), "live-agents.json")
}

// HooksInstalledPath marks that amux has installed Claude Code's status hooks,
// so setup only writes them once instead of on every launch.
func HooksInstalledPath() string {
	return filepath.Join(StateDir(), "hooks.installed")
}
