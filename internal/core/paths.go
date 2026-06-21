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

// ConfigDir is where the isolated tmux config lives.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "amux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "amux")
}

// TmuxConfPath is the isolated server's config file.
func TmuxConfPath() string {
	return filepath.Join(ConfigDir(), "amux.conf")
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

// WorkspacesDir holds per-workspace directories of repo worktrees.
func WorkspacesDir() string { return filepath.Join(DataDir(), "workspaces") }

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
