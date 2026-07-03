package agent

import (
	"context"
	"path/filepath"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/git"
	"amux/internal/store"
)

// claudeHarness implements Harness for Claude Code — the default harness. It maps
// Claude's hook-reported activity state, its on-disk transcript convention, its
// --resume/--session-id/--continue launch dance, and its config layout onto the
// abstract Harness primitives.
type claudeHarness struct{}

func (claudeHarness) Kind() string { return "claude" }

// claudeModels is the selectable Claude model catalog, opus first (the default).
var claudeModels = []string{"opus", "sonnet", "haiku", "fable"}

func (claudeHarness) Models() []string       { return claudeModels }
func (claudeHarness) DefaultModel() string   { return claudeModels[0] }
func (claudeHarness) PreferredModel() string { return claudecfg.PreferredModel() }

// Argv builds Claude Code's launch argv. It defaults to the safe auto-accept
// permission mode (a classifier blocks escalations — NOT
// --dangerously-skip-permissions); override with AMUX_PERMISSION_MODE=
// default|acceptEdits|plan|… or "none" to omit.
func (claudeHarness) Argv(model string, extra ...string) ([]string, error) {
	bin := envOr("AMUX_CLAUDE_BIN", "claude")
	var args []string
	if pm := envOr("AMUX_PERMISSION_MODE", "auto"); pm != "" && pm != "none" {
		args = append(args, "--permission-mode", pm)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return finishArgv(bin, args, extra), nil
}

// NewSessionID pre-mints a uuid: Claude accepts a pre-minted --session-id, so
// pinning one at creation gives durable resume across restarts.
func (claudeHarness) NewSessionID() string { return store.NewUUID() }

// PlanLaunch decides resume vs continue vs fresh for a Claude session. A pinned
// conversation resumes under the exact cwd its transcript lives under (that's the
// one Claude's own path munge will match); only --resume when the transcript is
// really there, since --session-id errors if the id is already known to Claude.
func (claudeHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	s := req.Session
	switch {
	case s.ClaudeID != "":
		if cwd, ok := claudecfg.FindSession(s.ClaudeID, req.ResumeCwds...); ok {
			core.ClearNotice(s.ClaudeID)
			return LaunchDecision{Dir: cwd, Extra: []string{"--resume", s.ClaudeID}}
		}
		// Pinned but no transcript under any candidate path: don't silently start
		// fresh — make the fallback visible in the log and on the rail.
		warnResumeFailed(req)
		return LaunchDecision{Dir: req.Dir, Extra: append([]string{"--session-id", s.ClaudeID}, freshExtra(req.Prompt)...)}
	case claudecfg.AnySession(req.Dir):
		return LaunchDecision{Dir: req.Dir, Extra: []string{"--continue"}}
	default:
		return LaunchDecision{Dir: req.Dir, Extra: freshExtra(req.Prompt)}
	}
}

// PrepareLaunchDir trusts the launch dir and installs amux's status/capture hooks
// into it (not the user-wide settings.json), pointed at the stable installed
// binary. Claude loads settings.local.json only from the launch dir. When the dir
// is a git repo (resuming a legacy conversation into a worktree), git-exclude the
// file so it never dirties the tree.
func (claudeHarness) PrepareLaunchDir(dir string) {
	_ = claudecfg.TrustDir(dir)
	if err := claudecfg.InstallHooksIn(dir, core.InstalledBinPath()); err == nil {
		if git.IsGitRepo(context.Background(), dir) {
			_ = git.Exclude(context.Background(), dir, ".claude/settings.local.json")
		}
	}
}

// AgentConfigBinds mounts Claude's config/auth writable — it stores transcripts
// under ~/.claude and reads ~/.claude.json.
func (claudeHarness) AgentConfigBinds(home string) [][]string {
	j := filepath.Join
	return [][]string{
		{"--bind-try", j(home, ".claude.json"), j(home, ".claude.json")},
		{"--bind-try", j(home, ".claude"), j(home, ".claude")},
	}
}

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

// RailState returns Claude's fine-grained hook state directly (running/waiting/
// ready/idle), or Unknown when no hook data has arrived yet — a granularity the
// coarse Activity signal can't preserve.
func (claudeHarness) RailState(sessionID string) string {
	if rec, ok := core.HookState(sessionID); ok {
		switch rec.State {
		case core.StateRunning, core.StateWaiting, core.StateReady, core.StateIdle:
			return rec.State
		}
	}
	return core.StateUnknown
}

// RestoreTranscript copies amux's captured backup of sessionID's transcript into
// the path Claude expects for cwd, when Claude's own copy is missing or staler.
// Because that is the exact location resume detection reads, a successful restore
// makes the subsequent launch resume the conversation via --resume.
func (claudeHarness) RestoreTranscript(cwd, sessionID string) (bool, error) {
	if cwd == "" || sessionID == "" {
		return false, nil
	}
	return core.RestoreCapturedTranscript(sessionID, claudecfg.TranscriptPath(cwd, sessionID))
}

// SkillsDir / GuideFile: Claude Code's own conventions — .claude/skills and
// CLAUDE.md, both under the launch dir.
func (claudeHarness) SkillsDir(root string) string { return filepath.Join(root, ".claude", "skills") }
func (claudeHarness) GuideFile(root string) string { return filepath.Join(root, "CLAUDE.md") }

// ListSessions maps Claude Code's transcript listing into the kind-neutral shape.
func (claudeHarness) ListSessions() []SessionInfo {
	src := claudecfg.ListSessions()
	out := make([]SessionInfo, 0, len(src))
	for _, s := range src {
		out = append(out, SessionInfo{
			ID: s.ID, Cwd: s.Cwd, Project: s.Project, Path: s.Path, Size: s.Size, Modified: s.Modified,
		})
	}
	return out
}

// RuntimeTranscriptPath resolves a Claude session to its transcript. It prefers
// the dir the transcript actually lives under (amux's dir convention has shifted
// over time), falling back to the recorded dir.
func (claudeHarness) RuntimeTranscriptPath(s store.Session) (string, bool) {
	if s.ClaudeID == "" {
		return "", false
	}
	if cwd, found := claudecfg.FindSession(s.ClaudeID, s.Dir); found {
		return claudecfg.TranscriptPath(cwd, s.ClaudeID), true
	}
	if s.Dir != "" {
		return claudecfg.TranscriptPath(s.Dir, s.ClaudeID), true
	}
	return "", false
}

// Doctor checks the load-bearing Claude project-dir path munge against Claude's
// actual on-disk layout: a discovered transcript whose real project dir differs
// from what amux computes means the munge convention drifted upstream (resume,
// capture, and listing would silently degrade). Empty when they agree.
func (claudeHarness) Doctor() []string { return claudecfg.MungeDrift() }
