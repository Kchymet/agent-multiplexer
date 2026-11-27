package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

// cmdAgentQuestion is the Claude question hold's hook binding
// (docs/question-prompts.md §4.1): `amux agent question hold` runs as a
// blocking PreToolUse hook on AskUserQuestion.
func cmdAgentQuestion(args []string) error {
	if len(args) == 0 || args[0] != "hold" {
		return fmt.Errorf("usage: amux agent question hold   (Claude's AskUserQuestion PreToolUse hook)")
	}
	runQuestionHold(os.Stdout, loadAgentSessionContextID, holdQuery, time.Now)
	return nil // fail open: the hook never fails Claude's tool call
}

// loadAgentSessionContextID is the caller's own session id.
func loadAgentSessionContextID() (string, error) { return selfAgentID(nil, loadAgentSessionContext) }

// holdQuery is one QueryQuestionHold round over the fixed session context.
func holdQuery(id string, fields map[string]string) (core.QuestionHoldResult, error) {
	var res core.QuestionHoldResult
	err := restrictedQueryRequest(sessionrpc.Query{Verb: core.QueryQuestionHold, ID: id, Fields: fields}, &res)
	return res, err
}

// holdHardStop is how long before Claude's own hook timeout the hook stops
// polling on its own, whatever the daemon says.
const holdHardStop = 30 * time.Second

// runQuestionHold reads the PreToolUse payload, then polls the daemon until
// the call is answered (allow with updatedInput), dismissed (deny with a
// reason) or released (print nothing: Claude draws its own dialog). Every
// failure releases: amux being unreachable must never cost the user the
// dialog, and the hook never exits 2 (which would block the call).
func runQuestionHold(out io.Writer, self func() (string, error), query func(string, map[string]string) (core.QuestionHoldResult, error), now func() time.Time) {
	var p claudecfg.HookPayload
	if err := readHookJSON(&p); err != nil {
		return
	}
	writeQuestionHold(out, p, self, query, now)
}

// writeQuestionHold is runQuestionHold after the payload is read.
func writeQuestionHold(out io.Writer, p claudecfg.HookPayload, self func() (string, error), query func(string, map[string]string) (core.QuestionHoldResult, error), now func() time.Time) {
	if p.ToolName != claudecfg.QuestionTool || p.ToolUseID == "" || len(p.ToolInput) == 0 || len(p.ToolInput) > core.MaxHoldInputBytes {
		return
	}
	id, err := self()
	if err != nil {
		return
	}
	start := now()
	stop := start.Add(time.Duration(claudecfg.QuestionHoldHookTimeout)*time.Second - holdHardStop)
	fields := map[string]string{
		core.HoldToolUseField: p.ToolUseID,
		core.HoldInputField:   string(p.ToolInput),
		core.HoldTimeoutField: strconv.Itoa(core.MaxWaitSeconds),
	}
	for now().Before(stop) {
		res, err := query(id, fields)
		if err != nil {
			return
		}
		switch res.Action {
		case core.HoldAllow:
			if len(res.UpdatedInput) == 0 || !json.Valid(res.UpdatedInput) {
				return
			}
			writeHookDecision(out, "allow", "", res.UpdatedInput)
			return
		case core.HoldDeny:
			writeHookDecision(out, "deny", res.Reason, nil)
			return
		case core.HoldWait:
			if res.Deadline > 0 && !now().Before(time.UnixMilli(res.Deadline)) {
				return
			}
			continue
		default: // release, or anything this binary does not know
			return
		}
	}
}

// writeHookDecision prints a PreToolUse decision.
func writeHookDecision(out io.Writer, decision, reason string, updated json.RawMessage) {
	hso := map[string]any{"hookEventName": "PreToolUse", "permissionDecision": decision}
	if reason != "" {
		hso["permissionDecisionReason"] = reason
	}
	if len(updated) > 0 {
		hso["updatedInput"] = updated
	}
	b, err := json.Marshal(map[string]any{"hookSpecificOutput": hso})
	if err != nil {
		return
	}
	_, _ = out.Write(append(b, '\n'))
}
