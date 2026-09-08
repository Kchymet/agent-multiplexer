package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/sessionreport"
)

// cmdAgent namespaces the commands an agent runs to describe *itself* to the
// harness: reporting its activity state and setting its display name. Unlike the
// management verbs (workgroup/do), which act on some other agent by id,
// these are scoped to the caller and resolve its own identity implicitly.
//
// Report identity comes only from the fixed authenticated sessionrpc context.
// Hook stdin and environment values are untrusted telemetry and cannot select a
// subject, runtime, path, journal, or permission target.
func cmdAgent(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "status":
		return cmdAgentStatus(args[1:], false)
	case "hook":
		return cmdAgentStatus(args[1:], true)
	case "capture":
		return cmdAgentCapture(args[1:])
	case "permission":
		// Claude-hook binding for nonauthoritative permission diagnostics.
		return cmdAgentPermission(args[1:])
	case "model":
		// Claude status-line binding: record the current model, then faithfully
		// forward the same payload to any status-line command amux wrapped.
		return cmdAgentModel(args[1:])
	case "sessions":
		// List every agent session on the machine (Claude Code + Codex) so an agent
		// can reason across conversations (not scoped to the caller — shared context).
		return cmdAgentSessions(args[1:])
	case "events":
		// Authenticated bounded history. The daemon derives and opens the source;
		// this client never receives a transcript path.
		return cmdAgentEvents(args[1:])
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

Scoped to the calling agent by the fixed authenticated session context. Stdin,
environment variables, UUIDs, and paths never select identity or storage.
Generated hook forms are best-effort so telemetry never disrupts the agent;
explicit report and control commands return nonzero when the daemon did not
confirm the requested operation.

usage: amux agent <command>

  status <state>     strictly report activity: idle | ready | waiting | running
  hook <state>       nondisruptive Claude-hook activity binding
  permission <verb>  report a diagnostic permission observation:
                     request | allow | deny | clear. Generated hooks add --hook.
                     These reports never create answerable approval rights.
  capture <event>    request a daemon-owned transcript snapshot; generated
                     Claude hooks use capture --hook and supply only the event
  model <model>      strictly report the runtime's current model
  model --statusline  report the model from Claude's status-line JSON. Claude's
                     installed status-line wrapper invokes this automatically
                     and preserves any pre-existing status-line command.
  name <text>        set this agent's display name  (alias: label)
  label <text>       alias of "name"
  done               report the task complete: archive this agent off the active
                     rail (reversible — amux workgroup unarchive <id>).
	                     identity comes only from the fixed session context
	  sessions [--json]  list host-visible agent sessions (legacy host diagnostic)
	  events [<id>]      read one bounded normalized event page through authenticated
	                     session RPC (--after/--cursor, --json)

Further self-reporting channels (topic, progress, attention, fields) are
specified in docs/agent-protocol.md and planned.
`)
}

// cmdAgentModel consumes Claude Code's status-line JSON, records its current
// model, and optionally runs the status-line command that amux wrapped. A status
// line is the only Claude callback whose payload follows `/model` changes; normal
// hooks carry the model only at SessionStart. Failures are swallowed only in
// --statusline mode; an explicit model report returns its exact failure.
func cmdAgentModel(args []string) error {
	var forward64, explicitModel string
	statusLine := false
	for _, arg := range args {
		if arg == "--statusline" {
			statusLine = true
		}
	}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--statusline":
			// Hook mode was detected before parsing so malformed generated
			// invocations remain nondisruptive regardless of flag order.
		case args[i] == "--forward-base64":
			if i+1 >= len(args) {
				return hookResult(statusLine, fmt.Errorf("amux agent model: --forward-base64 requires a value"))
			}
			forward64 = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--forward-base64="):
			forward64 = strings.TrimPrefix(args[i], "--forward-base64=")
		case strings.HasPrefix(args[i], "--"):
			return hookResult(statusLine, fmt.Errorf("amux agent model: unknown option %q", args[i]))
		case explicitModel == "":
			explicitModel = args[i]
		default:
			return hookResult(statusLine, fmt.Errorf("amux agent model: unexpected argument %q", args[i]))
		}
	}
	if !statusLine {
		if forward64 != "" {
			return fmt.Errorf("amux agent model: --forward-base64 requires --statusline")
		}
		if strings.TrimSpace(explicitModel) == "" {
			return fmt.Errorf("amux agent model: model is required")
		}
		return restrictedReport(sessionreport.Model, map[string]string{sessionreport.FieldModel: explicitModel})
	}
	if explicitModel != "" {
		return nil
	}

	input, overflow, readErr := readBoundedHookInput()
	var payload claudecfg.StatusLinePayload
	if readErr == nil && !overflow && decodeHookJSON(input, &payload) == nil {
		_ = restrictedReport(sessionreport.Model, map[string]string{sessionreport.FieldModel: payload.Model.ID})
	}

	if forward64 == "" {
		return nil
	}
	forward, err := base64.RawURLEncoding.DecodeString(forward64)
	if err != nil || strings.TrimSpace(string(forward)) == "" {
		return nil
	}
	cmd := exec.Command("sh", "-c", string(forward))
	// readBoundedHookInput consumes at most limit+1. If the input was oversized,
	// stitch that prefix back to the unread tail so the wrapped status command
	// receives the exact original byte stream without unbounded buffering.
	cmd.Stdin = io.MultiReader(bytes.NewReader(input), os.Stdin)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	_ = cmd.Run()
	return nil
}

// cmdAgentStatus reports activity through fixed-context RPC. Explicit status
// returns errors; the generated hook form validates bounded JSON but ignores its
// identity/path fields and remains intentionally nondisruptive.
func cmdAgentStatus(args []string, hook bool) error {
	if len(args) != 1 {
		return hookResult(hook, fmt.Errorf("activity state is required"))
	}
	if hook {
		var payload claudecfg.HookPayload
		if err := readHookJSON(&payload); err != nil {
			return nil
		}
	}
	return hookResult(hook, restrictedReport(sessionreport.Activity,
		map[string]string{sessionreport.FieldState: args[0]}))
}

// cmdAgentPermission reports the lifecycle a Claude hook claims it observed.
// These are diagnostics only: the session can fabricate an identical report, so
// neither authentication nor generation correlation can turn one into an
// answerable permission occurrence.
//
// The verbs mirror the hook events claudecfg binds (claudecfg.permissionHooks):
// `request` opens one, `allow`/`deny` close it with that decision, and `clear`
// retires whatever is still open at a turn boundary. Identity and the tool being
// asked about come from hook JSON. Generated --hook calls swallow errors;
// explicit diagnostics preserve validation and transport failures.
func cmdAgentPermission(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("amux agent permission: verb is required")
	}
	verb := args[0]
	// Settings generated before authenticated reports did not include --hook.
	// Preserve only that exact stdin-fed shape as nondisruptive compatibility.
	hook := len(args) == 1 && stdinPiped()
	// Generated hook invocations must stay nondisruptive even when a future
	// producer inserts an invalid flag before the exact --hook spelling.
	for _, arg := range args[1:] {
		if arg == "--hook" {
			hook = true
			break
		}
	}
	fields := make(map[string]string)
	for i := 1; i < len(args); i++ {
		name, value, consumed, err := reportFlag(args, i)
		if err != nil {
			return hookResult(hook, err)
		}
		if name == "hook" {
			hook = true
		} else {
			fields[name] = value
		}
		i += consumed
	}
	var payload claudecfg.HookPayload
	if hook {
		if err := readHookJSON(&payload); err != nil {
			return nil
		}
		if expected, ok := claudecfg.PermissionHookVerb(payload.HookEventName); !ok || expected != verb {
			return nil
		}
		fields[sessionreport.FieldTool] = payload.ToolName
	}
	var reportVerb string
	switch verb {
	case claudecfg.PermissionVerbRequest:
		reportVerb = sessionreport.PermissionRequest
		if hook {
			fields[sessionreport.FieldAction] = claudecfg.SummarizeToolInput(payload.ToolInput)
		}
		if fields[sessionreport.FieldRequestID] == "" {
			fields[sessionreport.FieldRequestID] = core.NewPermissionID()
		}
		options, _ := json.Marshal([]string{core.PermissionAllow, core.PermissionDeny})
		fields[sessionreport.FieldOptions] = string(options)
	case core.PermissionAllow, core.PermissionDeny:
		reportVerb = sessionreport.PermissionResolved
		fields[sessionreport.FieldDecision] = verb
	case claudecfg.PermissionVerbClear:
		reportVerb = sessionreport.PermissionClear
		if hook {
			delete(fields, sessionreport.FieldTool)
		}
		fields[sessionreport.FieldDecision] = core.PermissionCleared
	default:
		return hookResult(hook, fmt.Errorf("unknown permission observation verb %q", verb))
	}
	return hookResult(hook, restrictedReport(reportVerb, fields))
}

// cmdAgentDone is the terminal self-report: an agent declares its task finished
// and archives its own session so it drops off the active rail. It is the
// self-scoped form of the management verb `amux workgroup archive <id>` —
// instead of taking an id, it resolves the caller's own store id solely from the
// fixed authenticated session context.
//
// Unlike telemetry hooks, this durable control operation must report failure:
// exit 0 means the daemon confirmed archival. Archiving is reversible (`amux
// workgroup unarchive <id>`); it hides the row and does not delete the worktree
// or branch.
var loadAgentSessionContext = access.LoadSessionContext

func cmdAgentDone(args []string) error {
	id, err := selfAgentID(args, loadAgentSessionContext)
	if err != nil {
		return fmt.Errorf("amux agent done: %w", err)
	}
	if err := sendAction(core.Action{
		Action: core.ActionSetArchived,
		ID:     id,
		Fields: map[string]string{"archived": "true"},
	}); err != nil {
		return fmt.Errorf("amux agent done: archive %s: %w", id, err)
	}
	fmt.Printf("marked done: archived %s (reversible: amux workgroup unarchive %s)\n", id, id)
	return nil
}

// selfAgentID resolves the store id of the agent issuing a self-scoped control
// report (currently `done`). It accepts no caller identity: the fixed regular
// session context is the only source of the authenticated subject.
//
// This is the *store* subject id (used by archive/rename), distinct from the
// stored runtime id paired with it for activity/model/capture records.
func selfAgentID(args []string, load func() (access.SessionContext, error)) (string, error) {
	if len(args) != 0 {
		return "", fmt.Errorf("self completion accepts no id; identity comes only from %s", core.SessionContextPath())
	}
	context, err := load()
	if err != nil || strings.TrimSpace(context.SubjectID) == "" {
		return "", fmt.Errorf("%s", notInsideAgent("amux agent done", "amux workgroup archive <id>"))
	}
	return context.SubjectID, nil
}

// cmdAgentCapture asks the daemon to snapshot the authenticated subject's
// authoritative Claude transcript on a known hook event. Hook UUID/path fields
// are ignored; the daemon derives and securely opens the source. This is a
// diagnostic for the "restarting" bug: it gives us a durable copy of the
// conversation at turn/tool boundaries to compare against the transcript Claude
// Code persists itself. Generated --hook use is nondisruptive; an explicit
// capture reports errors.
func cmdAgentCapture(args []string) error {
	// The empty stdin-fed spelling is retained for settings generated before
	// --hook made the nondisruptive mode explicit.
	hook := (len(args) == 1 && args[0] == "--hook") || (len(args) == 0 && stdinPiped())
	event := ""
	if hook {
		var payload claudecfg.HookPayload
		if err := readHookJSON(&payload); err != nil {
			return nil
		}
		event = payload.HookEventName
	} else if len(args) == 1 {
		event = args[0]
	} else {
		return fmt.Errorf("amux agent capture: event is required")
	}
	return hookResult(hook, restrictedReport(sessionreport.Capture,
		map[string]string{sessionreport.FieldEvent: event}))
}

func hookResult(hook bool, err error) error {
	if hook {
		return nil
	}
	return err
}

func readBoundedHookInput() ([]byte, bool, error) {
	if !stdinPiped() {
		return nil, false, fmt.Errorf("hook input is required")
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, int64(sessionreport.MaxHookInputBytes)+1))
	return b, len(b) > sessionreport.MaxHookInputBytes, err
}

func readHookJSON(dst any) error {
	b, overflow, err := readBoundedHookInput()
	if err != nil {
		return err
	}
	if overflow {
		return fmt.Errorf("hook input exceeds %d bytes", sessionreport.MaxHookInputBytes)
	}
	return decodeHookJSON(b, dst)
}

func decodeHookJSON(b []byte, dst any) error {
	if len(b) == 0 || json.Unmarshal(b, dst) != nil {
		return fmt.Errorf("invalid hook JSON")
	}
	return nil
}

func reportFlag(args []string, i int) (name, value string, consumed int, err error) {
	arg := args[i]
	if arg == "--hook" {
		return "hook", "", 0, nil
	}
	for flag, field := range map[string]string{
		"--request-id": sessionreport.FieldRequestID,
		"--tool":       sessionreport.FieldTool,
		"--action":     sessionreport.FieldAction,
	} {
		if arg == flag {
			if i+1 >= len(args) {
				return "", "", 0, fmt.Errorf("%s requires a value", flag)
			}
			return field, args[i+1], 1, nil
		}
		if strings.HasPrefix(arg, flag+"=") {
			return field, strings.TrimPrefix(arg, flag+"="), 0, nil
		}
	}
	return "", "", 0, fmt.Errorf("unknown permission observation argument %q", arg)
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
