package agent

import (
	"path/filepath"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
)

// Harness abstracts one agent CLI (Claude Code is the built-in implementation)
// so amux's durability machinery — turn-aware shutdown and transcript gap-fill —
// works for any harness instead of hardcoding Claude. Launching the process is
// already abstracted by Argv; Harness adds the two primitives that machinery
// needs beyond a launch:
//
//   - an activity/turn-state signal, so a graceful stop can wait for an instance
//     to leave a turn before terminating it (consumed by the engine as an
//     engine.ActivityFunc), and
//   - transcript restore, so a relaunch fills the gap a harness leaves when it is
//     killed mid-turn before persisting its transcript.
//
// The resume-vs-fresh launch decision is the harness's third durability concern;
// Claude's lives in wsops.AgentCommand + claudecfg and is reached through Argv's
// extra args, so it is not re-declared here. The interface is deliberately small:
// the goal is decoupling the engine/daemon from Claude, not a plugin framework.
type Harness interface {
	// Kind is the agent kind this harness serves, matching Argv's kind ("claude").
	Kind() string
	// Activity reports whether the instance with harness session id is mid-turn
	// (ActivityBusy, unsafe to stop) or idle/between turns (ActivitySafe). It
	// returns engine.ActivityUnknown when there is no signal for the session, so a
	// missing signal never blocks a shutdown.
	Activity(sessionID string) engine.Activity
	// RestoreTranscript gap-fills the harness's own transcript for sessionID under
	// cwd from amux's captured backup, when the harness's copy is missing or
	// staler than the backup, so a relaunch resumes the real conversation instead
	// of starting fresh. It returns whether it restored a transcript, and never
	// clobbers a fresher harness transcript.
	RestoreTranscript(cwd, sessionID string) (bool, error)

	// SkillsDir is the directory this harness discovers skills in, under the
	// workspace root — each CLI reads its own place, so amux installs its skill
	// library there. Claude Code reads <root>/.claude/skills; a provider with no
	// native location falls back to the vendor-neutral <root>/.agents/skills.
	SkillsDir(root string) string

	// GuideFile is the agent-guide file this harness reads, under the workspace
	// root, where amux writes the sandbox instructions. Claude Code reads
	// <root>/CLAUDE.md; others fall back to the vendor-neutral <root>/AGENTS.md.
	GuideFile(root string) string
}

// HarnessFor returns the Harness for an agent kind. "" and "claude" get the
// Claude harness (matching Argv's default); any other kind gets a no-op harness
// that supplies no durability signal — its process still launches via Argv, it
// simply opts out of turn-aware shutdown and gap-fill until it implements them.
func HarnessFor(kind string) Harness {
	switch kind {
	case "", "claude":
		return claudeHarness{}
	default:
		return noopHarness{kind: kind}
	}
}

// claudeHarness implements Harness for Claude Code, mapping its hook-reported
// activity state (core.hookstate) and its on-disk transcript convention
// (claudecfg + core capture store) onto the abstract primitives.
type claudeHarness struct{}

func (claudeHarness) Kind() string { return "claude" }

// Activity maps Claude's hook state to an engine.Activity: a turn in progress or
// blocked on the user (running/waiting) is Busy; a finished turn or exited agent
// (ready/idle) is Safe; anything else, or no record, is Unknown.
func (claudeHarness) Activity(sessionID string) engine.Activity {
	rec, ok := core.HookState(sessionID)
	if !ok {
		return engine.ActivityUnknown
	}
	switch rec.State {
	case core.StateRunning, core.StateWaiting:
		return engine.ActivityBusy
	case core.StateReady, core.StateIdle:
		return engine.ActivitySafe
	default:
		return engine.ActivityUnknown
	}
}

// RestoreTranscript copies amux's captured backup of sessionID's transcript into
// the path Claude expects for cwd (claudecfg.TranscriptPath), when Claude's own
// copy is missing or staler. Because that is the exact location resume detection
// reads, a successful restore makes the subsequent AgentCommand resume the
// conversation via --resume instead of starting fresh.
func (claudeHarness) RestoreTranscript(cwd, sessionID string) (bool, error) {
	if cwd == "" || sessionID == "" {
		return false, nil
	}
	return core.RestoreCapturedTranscript(sessionID, claudecfg.TranscriptPath(cwd, sessionID))
}

// SkillsDir / GuideFile: Claude Code's own conventions — it reads project skills
// from .claude/skills and its guide from CLAUDE.md (both under the launch dir).
func (claudeHarness) SkillsDir(root string) string { return filepath.Join(root, ".claude", "skills") }
func (claudeHarness) GuideFile(root string) string { return filepath.Join(root, "CLAUDE.md") }

// noopHarness is the Harness for a kind with no durability primitives yet: it
// reports no activity signal (always safe to stop) and never restores. It keeps
// callers free of nil checks — an unknown kind degrades to today's behavior.
type noopHarness struct{ kind string }

func (n noopHarness) Kind() string                                 { return n.kind }
func (noopHarness) Activity(string) engine.Activity                { return engine.ActivityUnknown }
func (noopHarness) RestoreTranscript(string, string) (bool, error) { return false, nil }

// SkillsDir / GuideFile: a provider with no declared convention gets the
// vendor-neutral Agent Skills layout (.agents/skills, AGENTS.md) — e.g. Codex.
func (noopHarness) SkillsDir(root string) string { return filepath.Join(root, ".agents", "skills") }
func (noopHarness) GuideFile(root string) string { return filepath.Join(root, "AGENTS.md") }
