package main

import (
	"fmt"
	"os"
	"os/exec"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
)

func cmdAuth(args []string) error {
	if len(args) == 0 || (len(args) == 1 && isHelpFlag(args[0])) {
		authUsage()
		return nil
	}
	run, ok := authSubcommands[args[0]]
	if !ok {
		return fmt.Errorf("unknown auth subcommand %q; want login, status, or restart", args[0])
	}
	return run(args[1:])
}

var authSubcommands = map[string]func([]string) error{
	"login":   authLogin,
	"status":  authStatus,
	"restart": authRestart,
}

func authUsage() {
	fmt.Fprint(os.Stderr, `amux auth — one Claude login for all amux sessions

usage: amux auth <command>

  login             sign in once, then queue running Claude agents to resume
                    with the shared login when they are idle
  status            show Claude's status for the shared login
  restart [--force]  queue running Claude agents to resume with current credentials
                    --force also interrupts busy or unknown-state agents

Run login from your host terminal. This creates a separate amux login; it does
not copy your existing refresh token. Claude manages refreshes in the shared
credential directory. Requires Claude's CLAUDE_SECURESTORAGE_CONFIG_DIR support.
Unknown-state and busy agents wait; use restart --force if they are stuck at login.
Reopen existing terminal/editor tabs to pick up the shared credential environment.
`)
}

func authRestart(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--force") {
		return fmt.Errorf("usage: amux auth restart [--force]")
	}
	if !claudecfg.SharedAuthEnabled() {
		return fmt.Errorf("no shared Claude login configured; run amux auth login first")
	}
	return reloadClaudeAuth(len(args) == 1)
}

func authStatus(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: amux auth status")
	}
	if !claudecfg.SharedAuthEnabled() {
		return fmt.Errorf("no shared Claude login configured; run amux auth login first")
	}
	return runClaudeAuth("status")
}

func authLogin(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: amux auth login")
	}
	if err := claudecfg.Login(func() error { return runClaudeAuth("login") }); err != nil {
		return err
	}
	fmt.Println("Shared Claude login saved. New Claude sessions will use it.")
	if err := reloadClaudeAuth(false); err != nil {
		return fmt.Errorf("login saved, but running sessions could not be queued: %w; run amux auth restart after updating the daemon", err)
	}
	return nil
}

func runClaudeAuth(verb string) error {
	// Resolve the installed Claude/wrapper exactly as agent launches do, but do
	// not pass model or permission flags to a CLI auth subcommand.
	argv, err := agent.Argv("claude", "")
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], "auth", verb)
	cmd.Dir = claudecfg.SharedAuthDir()
	cmd.Env = claudecfg.AuthCommandEnv(os.Environ())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude auth %s failed: %w", verb, err)
	}
	return nil
}

func reloadClaudeAuth(force bool) error {
	fields := map[string]string{}
	if force {
		fields["force"] = "true"
	}
	if err := sendAction(core.Action{Action: core.ActionAuthReload, Fields: fields}); err != nil {
		return err
	}
	fmt.Println("Claude agents queued to resume with the shared login; busy and unknown-state agents wait unless --force is set.")
	return nil
}
