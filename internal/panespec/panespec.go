// Package panespec resolves what to run for one tab of an agent: the Claude agent
// process, an editor, or a shell jailed to the agent's worktree. It is shared by
// the native TUI (legacy direct-spawn) and the multiplexer server (which hands the
// spec to a harness), so pane launch behavior stays identical everywhere.
package panespec

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"amux/internal/console"
	"amux/internal/store"
	"amux/internal/wsops"
)

// Tabs an agent exposes.
const (
	TabAgent    = 0
	TabEditor   = 1
	TabTerminal = 2
)

// Resolve returns the launch spec (working dir, extra env KEY=VALUE, argv) for a
// tab of the agent. Tab 0 is the agent; 1 is the editor; 2 is a jailed shell.
func Resolve(agentID string, tab int) (dir string, env, argv []string, err error) {
	dir, env, argv, err = agentCommand(agentID)
	if err != nil {
		return "", nil, nil, err
	}
	switch tab {
	case TabEditor:
		argv = []string{editorBin()}
	case TabTerminal:
		argv = terminalArgv(dir)
	}
	return dir, env, argv, nil
}

// agentCommand resolves the agent process spec by id (handling the console).
func agentCommand(id string) (dir string, env, argv []string, err error) {
	if id == console.ID {
		if err = console.Ensure(); err != nil {
			return "", nil, nil, err
		}
		return wsops.AgentCommand(console.Session())
	}
	db, err := store.Open()
	if err != nil {
		return "", nil, nil, err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "", nil, nil, fmt.Errorf("no such agent %q", id)
	}
	return wsops.AgentCommand(s)
}

// EditorBin is the configured editor, defaulting to nvim.
func editorBin() string { return envOr("AMUX_EDITOR", "nvim") }

// shellBin is the user's shell, defaulting to a sane fallback.
func shellBin() string { return envOr("SHELL", "/bin/bash") }

// terminalArgv returns the argv for the terminal tab. By default the shell is
// jailed to dir with bubblewrap: the system is read-only, only dir and a private
// /tmp are writable, HOME is dir, and nothing outside is visible. Falls back to a
// plain shell if bwrap is missing or AMUX_JAIL=off.
func terminalArgv(dir string) []string {
	if envOr("AMUX_JAIL", "on") != "off" {
		if bw, err := exec.LookPath("bwrap"); err == nil {
			return jailArgv(bw, dir)
		}
	}
	return []string{shellBin()}
}

// jailArgv builds a bubblewrap command confining the shell to dir.
func jailArgv(bwrap, dir string) []string {
	args := []string{
		bwrap,
		"--die-with-parent",
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
	}
	for _, p := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/opt", "/nix", "/home/linuxbrew", "/run"} {
		args = append(args, "--ro-bind-try", p, p)
	}
	args = append(args,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", dir, dir,
		"--chdir", dir,
		"--setenv", "HOME", dir,
		"--setenv", "AMUX_JAILED", "1",
		shellBin(),
	)
	return args
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
