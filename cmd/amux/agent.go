package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"amux/internal/core"
)

// cmdAgent namespaces the commands an agent runs to describe *itself* to the
// harness: reporting its activity state and setting its display name. Unlike the
// management verbs (workgroup/session/do), which act on some other agent by id,
// these are scoped to the caller and resolve its own identity implicitly.
//
// Identity today is inferred, not authenticated: from the Claude hook payload on
// stdin, then $AMUX_SESSION_ID, then the tmux window (see cmdAgentStatus and
// cmdName). Per-agent identity (authn + authz) is planned so these commands can
// be scoped to the agent that actually issued them.
func cmdAgent(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status", "hook":
		// "hook" is the Claude-settings binding name; "status" is the general
		// verb. Both record activity state; they differ only in where identity
		// comes from (stdin for hook, stdin/env/flag for status).
		return cmdAgentStatus(args[1:])
	case "name", "label":
		return cmdName(args[1:])
	case "", "help", "-h", "--help":
		agentUsage()
		return nil
	default:
		agentUsage()
		return fmt.Errorf("unknown agent subcommand %q", sub)
	}
}

func agentUsage() {
	fmt.Fprint(os.Stderr, `amux agent — commands an agent runs to describe itself to the harness

Scoped to the calling agent: they resolve the caller's own identity (from the
Claude hook payload on stdin, else $AMUX_SESSION_ID, else the tmux window)
rather than taking an id like the management verbs. Reporting never disrupts the
agent — it exits 0 even with no daemon and swallows its own errors.

usage: amux agent <command>

  status <state>     report activity state: idle | ready | waiting | running
                     identity: stdin session_id, else $AMUX_SESSION_ID,
                     else --session <id>
  hook <state>       Claude-hook binding of "status" (identity from the hook
                     JSON on stdin); amux wires this into Claude's settings.json
  name <text>        set this agent's display name  (alias: label)
  label <text>       alias of "name"

Further self-reporting channels (topic, progress, attention, fields) are
specified in docs/agent-protocol.md and planned. Per-agent identity (authn +
authz) is planned to properly scope every command in this namespace.
`)
}

// cmdAgentStatus records the agent's current activity state for the daemon's
// poll loop to surface in the rail. It is the general form of the Claude-hook
// binding ("amux agent hook <state>"): the state word is the first argument, and
// the session identity + cwd come from, in order, a Claude hook JSON payload on
// stdin, then $AMUX_SESSION_ID, then a --session flag. It must never disrupt the
// agent, so it swallows all errors and exits 0.
func cmdAgentStatus(args []string) error {
	var state, sessionFlag string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--session" && i+1 < len(args):
			sessionFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--session="):
			sessionFlag = strings.TrimPrefix(args[i], "--session=")
		case state == "":
			state = args[i]
		}
	}
	if state == "" {
		return nil
	}

	// Claude Code pipes its hook event as JSON on stdin; other callers leave it
	// empty. Only read when stdin is not a terminal, or a manually-typed
	// `amux agent status running` would block on the tty forever. We use just
	// session_id and cwd.
	var payload struct {
		SessionID string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}
	if stdinPiped() {
		if b, err := io.ReadAll(os.Stdin); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &payload)
		}
	}

	// Explicit override wins, then the harness-set env, then the hook payload
	// (see docs/agent-protocol.md §3). In practice these agree when present.
	sessionID := firstNonEmpty(sessionFlag, os.Getenv("AMUX_SESSION_ID"), payload.SessionID)
	cwd := payload.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	_ = core.WriteHookState(sessionID, state, cwd)
	return nil
}

// stdinPiped reports whether stdin is a pipe/file rather than a terminal, so we
// can read a hook payload without blocking on interactive input.
func stdinPiped() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) == 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
