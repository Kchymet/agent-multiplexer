package agent

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"amux/internal/cfghome"
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

// claudeModels is the built-in Claude model catalog — the aliases every Claude
// Code install accepts — opus first (the default).
var claudeModels = []string{"opus", "sonnet", "haiku", "fable"}

// Models is the built-in catalog plus whatever Claude Code has cached as
// additionally selectable for this account (claudecfg.Home.AdditionalModels —
// a preview or org-enabled model amux's catalog predates) and the user's
// configured model (.claude.json), so a model named by full id is offered too.
func (claudeHarness) Models() []string {
	user := claudecfg.User()
	return mergeModels(claudeModels, user.AdditionalModels(), []string{user.PreferredModel()})
}

// DefaultModel is the first-offered model — always the built-in opus, since
// the discovered extras are appended after the aliases.
func (h claudeHarness) DefaultModel() string { return h.Models()[0] }
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

// Config templates the user's Claude home into the agent's sandbox: the copy
// lives at <agent dir>/.amux/claude and CLAUDE_CONFIG_DIR points Claude at it
// (see claudecfg.Template for what is copied, compared, and shared).
func (claudeHarness) Config(s store.Session) (cfghome.Spec, bool) {
	if s.Dir == "" {
		return cfghome.Spec{}, false
	}
	return claudecfg.Template(s.ID, claudecfg.AgentHome(s.Dir)), true
}

// home is the agent's private Claude home — where its transcripts, trust, and
// settings live. An agent with no dir (a synthetic session in tests) falls back
// to the user's home so lookups still resolve somewhere sensible.
func (h claudeHarness) home(s store.Session) claudecfg.Home {
	if sp, ok := h.Config(s); ok {
		return claudecfg.At(sp.Dir)
	}
	return claudecfg.User()
}

// PlanLaunch decides resume vs continue vs fresh for a Claude session. A pinned
// conversation resumes under the exact cwd its transcript lives under (that's the
// one Claude's own path munge will match); only --resume when the transcript is
// really there, since --session-id errors if the id is already known to Claude.
//
// Transcripts live in the agent's private home. An agent created before homes
// were private has its conversation in the user's home; the first launch after
// the switch carries that project dir over (once), so the switch never costs a
// conversation.
func (h claudeHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	s := req.Session
	home := h.home(s)
	for _, cwd := range req.ResumeCwds {
		carryOver(home, cwd)
	}
	switch {
	case s.ClaudeID != "":
		if cwd, ok := home.FindSession(s.ClaudeID, req.ResumeCwds...); ok {
			core.ClearNotice(s.ClaudeID)
			return LaunchDecision{Dir: cwd, Extra: []string{"--resume", s.ClaudeID}}
		}
		// Pinned but no transcript under any candidate path: don't silently start
		// fresh — make the fallback visible in the log and on the rail.
		warnResumeFailed(req)
		return LaunchDecision{Dir: req.Dir, Extra: append([]string{"--session-id", s.ClaudeID}, freshExtra(req.Prompt)...)}
	case home.AnySession(req.Dir):
		return LaunchDecision{Dir: req.Dir, Extra: []string{"--continue"}}
	default:
		return LaunchDecision{Dir: req.Dir, Extra: freshExtra(req.Prompt)}
	}
}

// carryOver copies cwd's project dir (its transcripts and their per-session
// working areas) from the user's home into home when home has none for cwd and
// the user's does — the one-time migration of a conversation recorded before the
// agent had a private home. A home that is the user's own is left alone.
func carryOver(home claudecfg.Home, cwd string) {
	user := claudecfg.User()
	if home.Dir == user.Dir {
		return
	}
	dst := home.ProjectDir(cwd)
	if _, err := os.Stat(dst); err == nil {
		return
	}
	src := user.ProjectDir(cwd)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return
	}
	if err := copyTree(src, dst); err != nil {
		log.Printf("amux: carrying %s into the agent's config home: %v", src, err)
		return
	}
	log.Printf("amux: carried transcripts for %s into the agent's private Claude home", cwd)
}

// copyTree copies the regular files under src to dst, preserving modes.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if fi, err := d.Info(); err == nil {
			mode = fi.Mode().Perm()
		}
		return os.WriteFile(target, b, mode)
	})
}

// PrepareLaunch trusts the launch dir in the agent's own home and installs amux's
// status/capture hooks into the launch dir (not the user-wide settings.json),
// pointed at the stable installed binary. Claude loads settings.local.json only
// from the launch dir. When the dir is a git repo (resuming a legacy conversation
// into a worktree), git-exclude the file so it never dirties the tree.
func (h claudeHarness) PrepareLaunch(s store.Session, dir string) {
	_ = h.home(s).TrustDir(dir)
	if err := claudecfg.InstallHooksIn(dir, core.InstalledBinPath()); err == nil {
		if git.IsGitRepo(context.Background(), dir) {
			_ = git.Exclude(context.Background(), dir, ".claude/settings.local.json")
		}
	}
}

// Keys are Claude Code's interactive bindings (see claudeKeys).
func (claudeHarness) Keys() Keys { return claudeKeys() }

// Activity maps Claude's hook state to an engine.Activity: a turn in progress or
// blocked on the user (running/waiting) is Busy; a finished turn or exited agent
// (ready/idle) is Safe; anything else, or no record, is Unknown.
func (claudeHarness) Activity(s store.Session) engine.Activity {
	rec, ok := core.HookState(s.ClaudeID)
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
func (claudeHarness) RailState(s store.Session) string {
	if rec, ok := core.HookState(s.ClaudeID); ok {
		switch rec.State {
		case core.StateRunning, core.StateWaiting, core.StateReady, core.StateIdle:
			return rec.State
		}
	}
	return core.StateUnknown
}

// RestoreTranscript copies amux's captured backup of the session's transcript
// into the path Claude expects for cwd in the agent's home, when Claude's own
// copy is missing or staler. Because that is the exact location resume detection
// reads, a successful restore makes the subsequent launch resume the
// conversation via --resume.
func (h claudeHarness) RestoreTranscript(s store.Session, cwd string) (bool, error) {
	if cwd == "" || s.ClaudeID == "" {
		return false, nil
	}
	return core.RestoreCapturedTranscript(s.ClaudeID, h.home(s).TranscriptPath(cwd, s.ClaudeID))
}

// SkillsDir / GuideFile: Claude Code's own conventions — .claude/skills and
// CLAUDE.md, both under the launch dir.
func (claudeHarness) SkillsDir(root string) string { return filepath.Join(root, ".claude", "skills") }
func (claudeHarness) GuideFile(root string) string { return filepath.Join(root, "CLAUDE.md") }

// homes is every Claude home on the machine: the user's own, then each agent's
// private one.
func (h claudeHarness) homes() []claudecfg.Home {
	out := []claudecfg.Home{claudecfg.User()}
	for _, d := range agentDirs(h.Kind()) {
		out = append(out, claudecfg.At(claudecfg.AgentHome(d)))
	}
	return out
}

// ListSessions merges every Claude home's transcript listing (the user's and each
// agent's) into the kind-neutral shape, most recent first.
func (h claudeHarness) ListSessions() []SessionInfo {
	var out []SessionInfo
	seen := map[string]bool{}
	for _, home := range h.homes() {
		for _, s := range home.ListSessions() {
			if seen[s.Path] {
				continue
			}
			seen[s.Path] = true
			out = append(out, SessionInfo{
				ID: s.ID, Cwd: s.Cwd, Project: s.Project, Path: s.Path, Size: s.Size, Modified: s.Modified,
			})
		}
	}
	sortSessions(out)
	return out
}

// RuntimeTranscriptPath resolves a Claude session to its transcript in the
// agent's home. It prefers the dir the transcript actually lives under (amux's
// dir convention has shifted over time), falling back to the recorded dir. An
// agent that has not launched since config homes became private still has its
// transcript in the user's home; that path is answered until the next launch
// carries it over, so a live reader never loses the conversation in between.
func (h claudeHarness) RuntimeTranscriptPath(s store.Session) (string, bool) {
	if s.ClaudeID == "" {
		return "", false
	}
	home := h.home(s)
	if cwd, found := home.FindSession(s.ClaudeID, s.Dir); found {
		return home.TranscriptPath(cwd, s.ClaudeID), true
	}
	if user := claudecfg.User(); user.Dir != home.Dir {
		if cwd, found := user.FindSession(s.ClaudeID, s.Dir); found {
			return user.TranscriptPath(cwd, s.ClaudeID), true
		}
	}
	if s.Dir != "" {
		return home.TranscriptPath(s.Dir, s.ClaudeID), true
	}
	return "", false
}

// RuntimePermissionPath resolves a Claude session to amux's permission journal
// for it. Claude Code answers permission prompts in its TUI and records none of
// them in the transcript, so this journal — written by the hooks amux installs —
// is the only place a prompt is durable, and the only reason a remote consumer
// has a request_id to quote back. The path is answered whether or not the file
// exists yet: the reader tolerates a record that has not appeared.
func (h claudeHarness) RuntimePermissionPath(s store.Session) (string, bool) {
	if s.ClaudeID == "" {
		return "", false
	}
	return core.PermissionJournalPath(s.ClaudeID), true
}

// Doctor checks the load-bearing Claude project-dir path munge against Claude's
// actual on-disk layout in every home: a discovered transcript whose real project
// dir differs from what amux computes means the munge convention drifted upstream
// (resume, capture, and listing would silently degrade). Empty when they agree.
func (h claudeHarness) Doctor() []string {
	var out []string
	for _, home := range h.homes() {
		out = append(out, home.MungeDrift()...)
	}
	return out
}
