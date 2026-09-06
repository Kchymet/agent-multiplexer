package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
)

// cmdAgent namespaces the commands an agent runs to describe *itself* to the
// harness: reporting its activity state and setting its display name. Unlike the
// management verbs (workgroup/do), which act on some other agent by id,
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
	case "capture":
		// Claude-hook binding that snapshots the conversation transcript (identity
		// and transcript path come from the hook JSON on stdin).
		return cmdAgentCapture()
	case "permission":
		// Claude-hook binding that journals the runtime's permission prompts, which
		// the transcript never records (identity and tool come from the hook JSON on
		// stdin).
		return cmdAgentPermission(args[1:])
	case "model":
		// Claude status-line binding: record the current model, then faithfully
		// forward the same payload to any status-line command amux wrapped.
		return cmdAgentModel(args[1:])
	case "sessions":
		// List every agent session on the machine (Claude Code + Codex) so an agent
		// can reason across conversations (not scoped to the caller — shared context).
		return cmdAgentSessions(args[1:])
	case "name", "label":
		return cmdName(args[1:])
	case "done":
		// Terminal self-report: the agent declares its task complete and archives
		// itself off the active rail. The self-scoped analog of the management
		// verb `amux workgroup archive <id>`, resolving the caller's own identity.
		return cmdAgentDone(args[1:])
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
  permission <verb>  Claude-hook binding that journals permission prompts:
                     request | allow | deny | clear. amux wires this into
                     Claude's settings.json; the journal is what gives a remote
                     orchestrator a request_id to answer.
  model [--statusline]  report the runtime's current model. Claude's installed
                     status-line wrapper invokes this automatically and preserves
                     any pre-existing status-line command.
  name <text>        set this agent's display name  (alias: label)
  label <text>       alias of "name"
  done               report the task complete: archive this agent off the active
                     rail (reversible — amux workgroup unarchive <id>).
                     identity: $AMUX_WORKGROUP, else --id <id>
  sessions [--json]  list every agent session on this machine — Claude Code and
                     Codex, tagged by harness — most recent first, so you can
                     reason about work that spans conversations. Read a transcript
                     with your normal file tools; --json emits the full records.

Further self-reporting channels (topic, progress, attention, fields) are
specified in docs/agent-protocol.md and planned. Per-agent identity (authn +
authz) is planned to properly scope every command in this namespace.
`)
}

// cmdAgentModel consumes Claude Code's status-line JSON, records its current
// model, and optionally runs the status-line command that amux wrapped. A status
// line is the only Claude callback whose payload follows `/model` changes; normal
// hooks carry the model only at SessionStart. Like all self-reporting commands,
// failures are swallowed so telemetry can never interrupt the runtime.
func cmdAgentModel(args []string) error {
	var forward64 string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--forward-base64" && i+1 < len(args):
			forward64 = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--forward-base64="):
			forward64 = strings.TrimPrefix(args[i], "--forward-base64=")
		}
	}

	var input []byte
	if stdinPiped() {
		input, _ = io.ReadAll(os.Stdin)
	}
	var payload claudecfg.StatusLinePayload
	_ = json.Unmarshal(input, &payload)
	sessionID := firstNonEmpty(os.Getenv("AMUX_SESSION_ID"), payload.SessionID)
	_ = core.WriteRuntimeModel(sessionID, payload.Model.ID)

	if forward64 == "" {
		return nil
	}
	forward, err := base64.RawURLEncoding.DecodeString(forward64)
	if err != nil || strings.TrimSpace(string(forward)) == "" {
		return nil
	}
	cmd := exec.Command("sh", "-c", string(forward))
	cmd.Stdin = strings.NewReader(string(input))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	_ = cmd.Run()
	return nil
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

	// Claude Code pipes its hook event as JSON on stdin (the claudecfg.HookPayload
	// shape); other callers leave it empty. Only read when stdin is not a terminal,
	// or a manually-typed `amux agent status running` would block on the tty
	// forever. We use just session_id and cwd here.
	var payload claudecfg.HookPayload
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

// cmdAgentPermission journals the lifecycle of one permission prompt, the signal
// Claude Code's transcript does not carry: the prompt opens in the TUI, the human
// answers, and nothing reaches disk. amux's hooks are the only producer, so this
// is what lets the runtime-events stream publish a `permission_request` with a
// request_id and the daemon refuse a `permission` verb that names a prompt which
// has since been answered (docs/remote-provider-sessions.md §4.5).
//
// The verbs mirror the hook events claudecfg binds (claudecfg.permissionHooks):
// `request` opens one, `allow`/`deny` close it with that decision, and `clear`
// retires whatever is still open at a turn boundary. Identity and the tool being
// asked about come from the hook JSON on stdin. Like every `amux agent` verb it
// must never disrupt the agent, so it swallows all errors and exits 0.
func cmdAgentPermission(args []string) error {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	if verb == "" {
		return nil
	}

	var payload claudecfg.HookPayload
	if stdinPiped() {
		if b, err := io.ReadAll(os.Stdin); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &payload)
		}
	}
	sessionID := firstNonEmpty(os.Getenv("AMUX_SESSION_ID"), payload.SessionID)
	if sessionID == "" {
		return nil // not an identifiable session: nothing to journal against
	}

	switch verb {
	case claudecfg.PermissionVerbRequest:
		_ = core.AppendPermission(sessionID, core.PermissionRecord{
			RequestID: core.NewPermissionID(),
			Tool:      payload.ToolName,
			Action:    claudecfg.SummarizeToolInput(payload.ToolInput),
			// The offered options are what amux can actually deliver to the prompt
			// (agent.Keys), not every choice the TUI draws: a consumer must not be
			// shown a button the daemon has no keystroke for.
			Options: []string{core.PermissionAllow, core.PermissionDeny},
		})
	case core.PermissionAllow, core.PermissionDeny:
		// These hooks fire on every tool call, prompted or not; resolving nothing is
		// the common case and not an error.
		_, _ = core.ResolvePermission(sessionID, payload.ToolName, verb)
	case claudecfg.PermissionVerbClear:
		_ = core.ClearPermissions(sessionID)
	}
	return nil
}

// cmdAgentDone is the terminal self-report: an agent declares its task finished
// and archives its own session so it drops off the active rail. It is the
// self-scoped form of the management verb `amux workgroup archive <id>` —
// instead of taking an id, it resolves the caller's own store id (the one the
// harness sets in the agent's environment as $AMUX_WORKGROUP, see
// wsops.AgentCommand), so a one-off agent that has integrated its artifact can
// mark itself done without knowing its own id.
//
// Like every `amux agent` verb, reporting must never disrupt the agent: it
// swallows a missing identity and any harness error and always exits 0.
// Archiving is reversible (`amux workgroup unarchive <id>`); it hides the row
// and does not delete the worktree or branch.
func cmdAgentDone(args []string) error {
	id := selfAgentID(args, os.Getenv)
	if id == "" {
		// Not an amux-launched agent (or the env was stripped): nothing to archive.
		// A no-op, not an error — the agent shouldn't fail just because it wasn't
		// started by amux.
		fmt.Fprintln(os.Stderr, "amux agent done: "+notInsideAgent("amux agent done", "amux workgroup archive <id>"))
		return nil
	}
	if err := sendAction(core.Action{
		Action: core.ActionSetArchived,
		ID:     id,
		Fields: map[string]string{"archived": "true"},
	}); err != nil {
		// The harness was unreachable. Report it, but don't fail the agent — the
		// self-report is best-effort by contract.
		fmt.Fprintf(os.Stderr, "amux agent done: could not reach the harness to archive %s: %v\n", id, err)
		return nil
	}
	fmt.Printf("marked done: archived %s (reversible: amux workgroup unarchive %s)\n", id, id)
	return nil
}

// selfAgentID resolves the store id of the agent issuing a self-scoped control
// report (currently `done`). Precedence: an explicit --id flag, then the
// $AMUX_WORKGROUP the harness sets on every launched agent, then its legacy
// $AMUX_WORKSPACE alias. Empty when the caller isn't an amux-launched agent.
//
// This is the *store* session id (used by the archive/rename actions), distinct
// from the Claude session id that the activity-report verbs (status/hook) key on.
func selfAgentID(args []string, getenv func(string) string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--id" && i+1 < len(args):
			return strings.TrimSpace(args[i+1])
		case strings.HasPrefix(args[i], "--id="):
			return strings.TrimSpace(strings.TrimPrefix(args[i], "--id="))
		}
	}
	return firstNonEmpty(getenv("AMUX_WORKGROUP"), getenv("AMUX_WORKSPACE"))
}

// cmdAgentCapture snapshots the agent's Claude transcript into amux's own
// transcript dir, keyed by Claude's session id, on each hook event that carries a
// transcript_path (see claudecfg.captureEvents). This is a diagnostic for the
// "restarting" bug: it gives us a durable copy of the conversation, captured at
// every turn/tool boundary, to compare against the transcript Claude Code
// persists itself — so we can tell whether Claude's own copy was written and
// lost, or never written. Like status reporting, it must never disrupt the agent,
// so it swallows all errors and exits 0.
func cmdAgentCapture() error {
	var payload claudecfg.HookPayload
	if stdinPiped() {
		if b, err := io.ReadAll(os.Stdin); err == nil && len(b) > 0 {
			_ = json.Unmarshal(b, &payload)
		}
	}
	sessionID := firstNonEmpty(os.Getenv("AMUX_SESSION_ID"), payload.SessionID)
	_ = core.CaptureTranscript(sessionID, payload.TranscriptPath, payload.HookEventName, os.Getenv("AMUX_WORKGROUP"))
	return nil
}

// sessionRow is one agent conversation for `amux agent sessions`, merging the
// per-harness session listings into a single shape tagged with its harness so
// text and --json output stay consistent across Claude Code and Codex.
type sessionRow struct {
	Harness  string    `json:"harness"` // claude | codex
	ID       string    `json:"id"`
	Cwd      string    `json:"cwd"`
	Project  string    `json:"project"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// cmdAgentSessions lists every agent session on the machine — both Claude Code
// and Codex — so an agent can reason about tasks that recur across conversations:
// what other agents (or the user) worked on, prior decisions, common patterns.
// Unlike the other agent verbs this is not scoped to the caller: it's shared
// read-only context. Text output is one row per session (most recent first),
// tagged with its harness, with the path to read; --json emits the full records.
func cmdAgentSessions(args []string) error {
	asJSON := false
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		}
	}

	// Merge every registered harness's on-disk conversations, tagged by kind, so
	// adding a harness surfaces its sessions here without touching this command.
	var rows []sessionRow
	for _, h := range agent.Harnesses() {
		for _, s := range h.ListSessions() {
			rows = append(rows, sessionRow{
				Harness: h.Kind(), ID: s.ID, Cwd: s.Cwd, Project: s.Project,
				Path: s.Path, Size: s.Size, Modified: s.Modified,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Modified.After(rows[j].Modified) })

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no agent sessions found")
		return nil
	}
	for _, s := range rows {
		loc := s.Cwd
		if loc == "" {
			loc = s.Project // cwd unreadable; fall back to the storage grouping
		}
		fmt.Printf("%s  %-6s  %8s  %s\n    %s\n",
			s.Modified.Format("2006-01-02 15:04"), s.Harness, humanSize(s.Size), loc, s.Path)
	}
	return nil
}

// humanSize renders a byte count as a compact, human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
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
