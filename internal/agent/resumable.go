package agent

import (
	"amux/internal/hostprep"
	"amux/internal/store"
)

// ConversationSaved reports whether a PTY launch of s would resume an existing
// conversation, using the same evidence PlanLaunch resumes from (read only; no
// adoption side effects): for Claude Code, the pinned conversation's
// transcript under one of cwds; for Codex, the pinned rollout, or for a first
// run whose id amux has not adopted yet, a rollout under one of cwds in the
// agent's private home. Without that evidence a relaunch starts a new
// conversation, so callers that restart idle agents silently (harness updates,
// self-update) must leave such a session alone. why explains a false.
func ConversationSaved(root *hostprep.Root, s store.Session, cwds []string) (ok bool, why string) {
	switch Canonical(s.Agent) {
	case "claude":
		if s.ClaudeID == "" {
			return false, "no pinned conversation"
		}
		_, found, err := claudeHarness{}.home(s).FindSessionRooted(root, s.ClaudeID, cwds...)
		if err != nil {
			return false, "cannot read its transcript: " + err.Error()
		}
		if !found {
			return false, "no saved transcript yet (it has not run a turn)"
		}
		return true, ""
	case "codex":
		home := codexHarness{}.home(s)
		if s.ClaudeID != "" {
			_, found, err := home.RolloutPathRooted(root, s.ClaudeID)
			if err != nil {
				return false, "cannot read its rollout: " + err.Error()
			}
			if !found {
				return false, "no saved rollout yet (it has not run a turn)"
			}
			return true, ""
		}
		for _, cwd := range cwds {
			if _, found, err := home.LatestSessionRooted(root, cwd); err == nil && found {
				return true, ""
			}
		}
		return false, "no saved rollout yet (it has not run a turn)"
	}
	return false, "amux cannot resume " + Canonical(s.Agent) + " conversations"
}
