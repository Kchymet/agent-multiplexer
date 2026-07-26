package main

import (
	"strings"
	"testing"

	"amux/internal/core"
)

// TestCmdDoRejectsUnscriptableActions pins the screening cmdDo does before it
// dials: the read and streaming verbs answer with Data/pane frames a one-shot
// caller can't consume, so sending them would hang waiting for a Result that
// never comes. Each is refused with where to go instead.
func TestCmdDoRejectsUnscriptableActions(t *testing.T) {
	tests := []struct{ action, want string }{
		{core.ActionQuery, "amux workgroup ls"},
		{core.ActionPaneOpen, "dashboard"},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			err := cmdDo([]string{tt.action})
			if err == nil {
				t.Fatalf("cmdDo(%q) = nil error, want an error", tt.action)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("cmdDo(%q) error %q does not point at %q", tt.action, err, tt.want)
			}
		})
	}
}

// TestCmdDoUnknownActionTeaches covers `amux do <bad-action>`: it fails locally,
// before the daemon is dialed, and answers with the whole vocabulary.
func TestCmdDoUnknownActionTeaches(t *testing.T) {
	err := cmdDo([]string{"add-agnet", "9c1b"})
	if err == nil {
		t.Fatal("cmdDo(bogus verb) = nil error, want an error")
	}
	for _, want := range []string{`"add-agnet"`, "valid actions:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	for _, verb := range core.ControlActions() {
		if !strings.Contains(err.Error(), verb) {
			t.Errorf("error does not list the valid action %q:\n%v", verb, err)
		}
	}
}

// TestActionGlossesCoverVocabulary keeps the help honest: a verb added to
// core.ControlActions() still shows up in the list (that's what the test above
// checks), but glossless it teaches nothing — so require the wording too.
func TestActionGlossesCoverVocabulary(t *testing.T) {
	for _, verb := range core.ControlActions() {
		if strings.TrimSpace(actionGlosses[verb]) == "" {
			t.Errorf("action %q has no gloss in actionGlosses", verb)
		}
	}
	for verb := range actionGlosses {
		if !core.KnownAction(verb) {
			t.Errorf("actionGlosses documents %q, which is not in core.ControlActions()", verb)
		}
	}
}

// TestNotInsideAgentExplainsWhere pins the message a self-scoped command gives
// outside an agent: naming the unset variable is not enough — it has to say
// where the command does work, and how to name the target from anywhere else.
func TestNotInsideAgentExplainsWhere(t *testing.T) {
	msg := notInsideAgent("amux agent done", "amux workgroup archive <id>")
	for _, want := range []string{
		"$AMUX_WORKGROUP unset",
		"terminal tab",
		"amux workgroup archive <id>",
		"amux workgroup ls",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("notInsideAgent() = %q, missing %q", msg, want)
		}
	}
}
