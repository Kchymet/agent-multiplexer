package amuxcfg

import (
	"encoding/json"
	"fmt"

	"amux/internal/core"
)

// QuestionHoldKey is the config path of the Claude question hold switch
// (`questions.claude_hold`, docs/question-prompts.md §4.1): whether amux holds
// a Claude AskUserQuestion in its PreToolUse hook so any amux surface can
// answer it structurally, releasing it to the pane on demand. On by default.
// The daemon re-reads it for every question, so a change applies at once.
const QuestionHoldKey = "questions.claude_hold"

func questionsSection(top map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if b, ok := top["questions"]; ok {
		return object(b, core.ConfigPath()+": questions")
	}
	return map[string]json.RawMessage{}, nil
}

// QuestionHold returns the saved switch, or set=false when unconfigured.
func QuestionHold() (value, set bool, err error) {
	top, err := Read()
	if err != nil {
		return false, false, err
	}
	sec, err := questionsSection(top)
	if err != nil {
		return false, false, err
	}
	b, ok := sec["claude_hold"]
	if !ok {
		return false, false, nil
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return false, false, fmt.Errorf("%s: %s must be true or false", core.ConfigPath(), QuestionHoldKey)
	}
	return value, true, nil
}

// ResolveQuestionHold is the effective switch: the saved value, or on. An
// unreadable config reads as on (the hold fails open to the pane anyway).
func ResolveQuestionHold() bool {
	value, set, err := QuestionHold()
	return err != nil || !set || value
}

// SetQuestionHold saves the switch; nil unsets it (back to on).
func SetQuestionHold(value *bool) error {
	top, err := Read()
	if err != nil {
		return err
	}
	sec, err := questionsSection(top)
	if err != nil {
		return err
	}
	if value == nil {
		delete(sec, "claude_hold")
	} else {
		sec["claude_hold"], _ = json.Marshal(*value)
	}
	if len(sec) == 0 {
		delete(top, "questions")
	} else {
		top["questions"], _ = json.Marshal(sec)
	}
	return Write(top)
}
