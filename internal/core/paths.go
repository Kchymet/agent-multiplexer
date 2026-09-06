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

// ConfigDir is amux's user configuration directory (the shell shim and
// config.json live here). Honors $XDG_CONFIG_HOME.
func ConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "amux")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "amux")
}

// ConfigPath is the user-editable settings file for keybindings and daemon
// rollout settings. `amux config` reads and writes it.
func ConfigPath() string { return filepath.Join(ConfigDir(), "config.json") }

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

// AuthDir holds dedicated harness credential stores. Panes mask this root and
// expose only the store selected by their harness, rather than all credentials.
func AuthDir() string { return filepath.Join(DataDir(), "auth") }

// WorkspacesDir holds legacy per-workspace directories (pre-hierarchy).
func WorkspacesDir() string { return filepath.Join(DataDir(), "workspaces") }

// SessionsDir holds per-root-session directories of sub-session worktrees.
func SessionsDir() string { return filepath.Join(DataDir(), "sessions") }

// BranchPrefix namespaces every branch amux creates. Cleanup only ever deletes a
// branch under it, and doctor's reconciliation only ever looks under it.
const BranchPrefix = "amux/"

// BranchFor is the git branch an agent commits on: amux/<root>-<agent>. This is
// the single source of the branch scheme — every worktree, the agent guide, and
// the doctor reconciliation derive the branch from here, so changing the scheme is
// a one-line change (plus a doctor migration note for already-created branches).
//
// The name is flat (a hyphen, not a slash) so it never collides with a legacy
// per-workspace branch amux/<root>: git refs can't have both amux/<root> and
// amux/<root>/<agent> (a file/directory conflict), which broke adding a second
// agent to older workspaces.
func BranchFor(rootID, agentID string) string {
	return BranchPrefix + rootID + "-" + agentID
}

// LegacyBranchFor is the pre-hierarchy scheme, amux/<root>: one branch per
// workspace, checked out by every repo worktree under workspaces/<root>. A
// session imported from that layout carries no stored branch (see
// store.importLegacy), so delete derives the name from here when it can't read
// it off the worktree itself.
func LegacyBranchFor(rootID string) string { return BranchPrefix + rootID }

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

// InstalledBinPath is the stable, user-local amux binary ($HOME/.local/bin/amux,
// the Makefile's default install target). Claude Code's status hooks and the
// agent sandbox reference this rather than the running daemon's os.Executable()
// path: that path can be a throwaway dev build inside a session worktree which
// later vanishes, breaking the hooks — whereas the install path is stable and is
// exactly what the sandbox scope binds in (see panespec.configBinds).
func InstalledBinPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "amux")
}
