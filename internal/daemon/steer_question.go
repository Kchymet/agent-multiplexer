package daemon

import (
	"encoding/json"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/runtimeevents"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// claudeQuestionTool is the Claude Code tool whose call draws a selector in
// place of the composer until the user answers or cancels it.
const claudeQuestionTool = "AskUserQuestion"

// questionOpen reports whether sess has a structured question open in place
// of its composer. Tests replace d.questionOpen; production reads the
// session's own hook state and transcript.
func (d *Daemon) questionOpen(h agent.Harness, sess store.Session) bool {
	if d.questionOpenFn != nil {
		return d.questionOpenFn(h, sess)
	}
	return claudeQuestionOpen(h, sess)
}

// claudeQuestionOpen reports whether a Claude session is sitting on an
// AskUserQuestion selector: its turn is live (running or waiting — never a
// finished or exited one, so a transcript left with a dangling call by a crash
// cannot make amux press Esc at an idle composer) and its transcript's tail
// holds an AskUserQuestion call with no result yet.
func claudeQuestionOpen(h agent.Harness, sess store.Session) bool {
	if agent.Canonical(sess.Agent) != harnessproto.RuntimeClaude {
		return false
	}
	switch h.RailState(sess) {
	case core.StateRunning, core.StateWaiting:
	default:
		return false
	}
	path, ok := h.RuntimeTranscriptPath(sess)
	if !ok {
		return false
	}
	return claudeQuestionOpenIn(path)
}

// claudeQuestionOpenIn reports whether a Claude transcript's tail ends with an
// AskUserQuestion call still unanswered: no tool_result for it and no later
// user prompt (a new prompt means the selector is gone).
func claudeQuestionOpenIn(path string) bool {
	lines, ok := transcriptTail(path)
	if !ok {
		return false
	}
	st := &runtimeevents.ClaudeState{}
	open := map[string]bool{}
	for _, line := range lines {
		for _, ev := range runtimeevents.MapClaudeLine([]byte(line), st) {
			switch ev.Type {
			case harnessproto.TypeToolCall:
				var p struct {
					Title string `json:"title"`
				}
				if json.Unmarshal(ev.Payload, &p) == nil && p.Title == claudeQuestionTool && ev.ItemID != "" {
					open[ev.ItemID] = true
				}
			case harnessproto.TypeToolResult:
				delete(open, ev.ItemID)
			case harnessproto.TypeTurnStart:
				clear(open)
			}
		}
	}
	return len(open) > 0
}

// dismissingQuestion prefixes typed steering with the runtime's question
// dismissal, and holds the text until the composer is back. The payload is
// copied, never mutated.
func dismissingQuestion(keys agent.Keys, payload []engine.InputStep) []engine.InputStep {
	out := make([]engine.InputStep, 0, len(payload)+1)
	out = append(out, engine.InputStep{Bytes: append([]byte(nil), keys.DismissQuestion...)})
	out = append(out, payload...)
	if len(out) > 1 && out[1].DelayBefore < keys.DismissDelay {
		out[1].DelayBefore = keys.DismissDelay
	}
	return out
}
