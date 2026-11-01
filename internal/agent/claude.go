package agent

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"amux/internal/cfghome"
	"amux/internal/claudecfg"
	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/hostprep"
	"amux/internal/store"
)

// claudeHarness implements Harness for Claude Code — the default harness. It maps
// Claude's hook-reported activity state, its on-disk transcript convention, its
// --resume/--session-id/--continue launch dance, and its config layout onto the
// abstract Harness primitives.
type claudeHarness struct{}

func (claudeHarness) Kind() string { return "claude" }

// claudeModels is the built-in Claude model catalog: detailed model IDs, with
// Opus first (the default). amux passes these IDs to Claude Code verbatim so a
// launch names exactly the model the picker showed.
var claudeModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}

// claudeModelAliases maps Claude Code's short model aliases — which older amux
// releases offered and so may already be stored on a session, set in a user's
// .claude.json, or passed on the command line (`--model opus`, `-f model=opus`)
// — to the detailed ID amux now offers in their place.
var claudeModelAliases = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-1",
}

// CanonicalClaudeModel resolves a short Claude alias to its detailed model ID,
// preserving a context-window suffix (`opus[1m]` → `claude-opus-5-5[1m]`).
// Anything that is not a known alias — a detailed ID, a custom model, or "" —
// is returned trimmed but otherwise unchanged.
func CanonicalClaudeModel(model string) string {
	model = strings.TrimSpace(model)
	base, suffix := model, ""
	if i := strings.IndexByte(model, '['); i > 0 {
		base, suffix = model[:i], model[i:]
	}
	if id, ok := claudeModelAliases[strings.ToLower(base)]; ok {
		return id + suffix
	}
	return model
}

// canonicalClaudeModels applies CanonicalClaudeModel to each entry.
func canonicalClaudeModels(models []string) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = CanonicalClaudeModel(m)
	}
	return out
}

// Models is the built-in catalog plus whatever Claude Code has cached as
// additionally selectable for this account (claudecfg.Home.AdditionalModels —
// a preview or org-enabled model amux's catalog predates) and the user's
// configured model (.claude.json), so a model named by full id is offered too.
// Discovered aliases are resolved to detailed IDs so none is offered twice.
func (claudeHarness) Models() []string {
	user := claudecfg.User()
	return mergeModels(claudeModels,
		canonicalClaudeModels(user.AdditionalModels()),
		[]string{CanonicalClaudeModel(user.PreferredModel())})
}

// DefaultModel is the first-offered model — always the built-in Opus ID, since
// the discovered extras are appended after the catalog.
func (h claudeHarness) DefaultModel() string { return h.Models()[0] }

// PreferredModel is the user's configured Claude model, with a short alias
// resolved to its detailed ID.
func (claudeHarness) PreferredModel() string {
	return CanonicalClaudeModel(claudecfg.PreferredModel())
}

// CurrentModel prefers Claude's live status-line report (updated immediately by
// `/model`) and falls back to the latest assistant transcript entry for sessions
// launched before amux installed that callback.
func (h claudeHarness) CurrentModel(s store.Session) (string, bool) {
	if report, ok := core.SessionRuntimeModel(s.ID, s.ClaudeID); ok {
		return report.Model, true
	}
	if path, ok := h.RuntimeTranscriptPath(s); ok {
		return latestModelLine(path, claudeModelLine)
	}
	return "", false
}

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
	// A short alias stored by an older amux (or passed via -f model=opus) is
	// launched as the detailed ID the catalog now offers.
	if model = CanonicalClaudeModel(model); model != "" {
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
	return claudecfg.Template(s.ID, s.Dir), true
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
func (h claudeHarness) PlanLaunch(req LaunchRequest) (LaunchDecision, error) {
	s := req.Session
	home := h.home(s)
	for _, cwd := range req.ResumeCwds {
		if err := carryOver(req.Root, home, cwd); err != nil {
			return LaunchDecision{}, err
		}
	}
	if s.ClaudeID != "" {
		if cwd, ok, err := home.FindSessionRooted(req.Root, s.ClaudeID, req.ResumeCwds...); err != nil {
			return LaunchDecision{}, err
		} else if ok {
			core.ClearNotice(s.ClaudeID)
			return LaunchDecision{Dir: cwd, Extra: resumedExtra(req, "--resume", s.ClaudeID)}, nil
		}
		// Pinned but no transcript under any candidate path: don't silently start
		// fresh — make the fallback visible in the log and on the rail.
		warnResumeFailed(req)
		extra, err := h.launchPrompt(req)
		if err != nil {
			return LaunchDecision{}, err
		}
		return LaunchDecision{Dir: req.Dir, Extra: append([]string{"--session-id", s.ClaudeID}, extra...)}, nil
	}
	any, err := home.AnySessionRooted(req.Root, req.Dir)
	if err != nil {
		return LaunchDecision{}, err
	}
	if any {
		return LaunchDecision{Dir: req.Dir, Extra: resumedExtra(req, "--continue")}, nil
	}
	extra, err := h.launchPrompt(req)
	if err != nil {
		return LaunchDecision{}, err
	}
	return LaunchDecision{Dir: req.Dir, Extra: extra}, nil
}

// carryOver copies cwd's project dir (its transcripts and their per-session
// working areas) from the user's home into home when home has none for cwd and
// the user's does — the one-time migration of a conversation recorded before the
// agent had a private home. A home that is the user's own is left alone.
func carryOver(root *hostprep.Root, home claudecfg.Home, cwd string) error {
	user := claudecfg.User()
	if home.Dir == user.Dir {
		return nil
	}
	dst := home.ProjectDir(cwd)
	dstRel, err := root.Rel(dst)
	if err != nil {
		return err
	}
	if sub, err := root.Sub(dstRel, false, 0); err == nil {
		_ = sub.Close()
		return nil
	} else if hostprep.IsUnsafe(err) {
		return err
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	src := user.ProjectDir(cwd)
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return nil
	}
	if err := copyTreeRooted(root, src, dstRel); err != nil {
		return err
	}
	log.Printf("amux: carried transcripts for %s into the agent's private Claude home", cwd)
	return nil
}

func copyTreeRooted(root *hostprep.Root, src, dstRel string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dstRel, rel)
		if d.IsDir() {
			if target == "." {
				return nil
			}
			dir, err := root.Sub(target, true, 0o755)
			if err == nil {
				_ = dir.Close()
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if fi, err := d.Info(); err == nil {
			mode = fi.Mode().Perm()
		}
		err = root.AtomicWriteFrom(target, f, mode)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

// PrepareLaunch trusts the launch dir in the agent's own home and installs amux's
// status/capture hooks into the launch dir (not the user-wide settings.json),
// pointed at the stable installed binary — plus the goal-mode hooks for a
// workgroup coordinator, which always runs in goal mode (HookGoals). Claude loads settings.local.json only
// from the launch dir. All writes are anchored under the pinned session root;
// do not invoke host-side Git against session-writable metadata here. Legacy
// worktree cleanup remains an explicit migration concern.
func (h claudeHarness) PrepareLaunch(root *hostprep.Root, s store.Session, dir string) error {
	if err := h.home(s).TrustDirRooted(root, dir); err != nil {
		return err
	}
	return claudecfg.InstallHooksInRooted(root, dir, h.home(s).Dir, core.SessionBinPath(s.ID), HookGoals(s))
}

// Keys are Claude Code's interactive bindings (see claudeKeys).
func (claudeHarness) Keys() Keys { return claudeKeys() }

// Activity maps Claude's hook state to an engine.Activity: a turn in progress or
// blocked on the user (running/waiting) is Busy; a finished turn or exited agent
// (ready/idle) is Safe; anything else, or no record, is Unknown.
func (claudeHarness) Activity(s store.Session) engine.Activity {
	rec, ok := core.SessionHookState(s.ID, s.ClaudeID)
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
	if rec, ok := core.SessionHookState(s.ID, s.ClaudeID); ok {
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
func (h claudeHarness) RestoreTranscript(root *hostprep.Root, s store.Session, cwd string) (bool, error) {
	if cwd == "" || s.ClaudeID == "" {
		return false, nil
	}
	return restoreCapturedRooted(root, s.ID, s.ClaudeID, h.home(s).TranscriptPath(cwd, s.ClaudeID))
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

// RuntimePermissionPath is deliberately unavailable for Claude PTY sessions.
// Claude hooks are session-authenticated observations, not runtime-native proof
// that a permission UI prompt exists, so they cannot mint answerable approvals.
// The pane's own local UI remains the trusted path until a daemon-owned runtime
// approval bridge exists.
func (claudeHarness) RuntimePermissionPath(store.Session) (string, bool) { return "", false }

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

// launchPrompt is the initial prompt of a fresh Claude conversation. A Claude task
// session starts in goal mode, so its task becomes the active goal at launch
// rather than an ordinary first turn the model may end whenever it likes:
//
//   - A workgroup coordinator (HookGoals) runs amux's own goal loop. Its goal is
//     seeded into the state file here, before Claude starts, instead of being
//     left to the UserPromptSubmit hook: the positional launch prompt is not a
//     reliable UserPromptSubmit event, so a coordinator could otherwise start
//     with no goal and its Stop hook would let the first turn end. The prompt is
//     passed as typed, so a UserPromptSubmit that does fire re-establishes the
//     same objective (claudegoal.OnPrompt) and changes nothing.
//   - Any other task-mode session (a workgroup member) uses Claude Code's
//     native /goal: the launch prompt becomes `/goal <task>`, which installs
//     Claude's session-scoped goal Stop hook with the task as its condition and
//     starts working on it. Coordinators do not, because two goal Stop hooks
//     would contradict each other (amux's `goal wait`/`ask` let a stop through
//     that the native goal would block).
//
// An interactive session is the human's to drive, so its prompt is passed
// unchanged, as is a resume (PlanLaunch never calls this for one): an existing
// conversation keeps whatever goal it already has.
func (claudeHarness) launchPrompt(req LaunchRequest) ([]string, error) {
	s, prompt := req.Session, strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, nil
	}
	if HookGoals(s) {
		if err := seedHookGoal(req.Root, prompt); err != nil {
			return nil, err
		}
		return []string{prompt}, nil
	}
	if !NativeClaudeGoal(s) {
		return []string{prompt}, nil
	}
	cond, err := nativeGoalCondition(req.Root, s, prompt)
	if err != nil {
		return nil, err
	}
	return []string{GoalCommand(cond)}, nil
}

// NativeClaudeGoal reports whether a fresh launch of s starts Claude Code's
// native /goal with its task: a Claude task-mode session that is an ordinary
// agent (a workgroup member). Coordinators run amux's goal hooks (HookGoals)
// instead; console, repo homes and interactive sessions are human-driven.
func NativeClaudeGoal(s store.Session) bool {
	return Canonical(s.Agent) == HookGoalRuntime && s.Role() == store.RoleAgent &&
		store.NormalizeMode(s.Mode) == store.ModeTask
}

// GoalCommand is the launch prompt that sets condition as Claude Code's native
// goal.
func GoalCommand(condition string) string { return "/goal " + condition }

// MaxNativeGoalCondition is Claude Code's limit on a /goal condition, in
// JavaScript string length (UTF-16 code units); a longer one is refused with
// "Goal condition is limited to 4000 characters" and no goal is set.
const MaxNativeGoalCondition = 4000

// NativeGoalTaskRel is where a member's task is written, relative to its
// session dir, when it is too long to be a /goal condition itself.
var NativeGoalTaskRel = filepath.Join(".amux", "task.md")

// nativeGoalCondition is the /goal condition for a task: the task itself when
// it fits, otherwise a pointer to the full task written beside the session's
// other private amux state, so the goal is still set (and the model still
// reads the whole task) instead of /goal refusing the launch prompt.
func nativeGoalCondition(root *hostprep.Root, s store.Session, prompt string) (string, error) {
	if len(utf16.Encode([]rune(prompt))) <= MaxNativeGoalCondition {
		return prompt, nil
	}
	if err := root.AtomicWrite(NativeGoalTaskRel, []byte(prompt+"\n"), 0o600); err != nil {
		return "", err
	}
	return "Complete the task written in " + filepath.Join(s.Dir, NativeGoalTaskRel) +
		" — read the whole file first; it is your assigned task, too long to state here. The goal is met only when everything it asks for is done and verified.", nil
}

// seedHookGoal establishes prompt as a Claude coordinator's active goal before
// its first launch. A goal already on file is left alone: a fresh start after a
// lost transcript replays the creation prompt, and must not overwrite a goal
// the user has since replaced, paused or completed.
func seedHookGoal(root *hostprep.Root, prompt string) error {
	b, err := root.ReadFile(claudegoal.StateRel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cur, err := claudegoal.Decode(b)
	if err != nil {
		return err
	}
	if !cur.None() {
		return nil
	}
	next := claudegoal.OnPrompt(cur, prompt, time.Now())
	return root.AtomicWrite(claudegoal.StateRel, claudegoal.Encode(next), 0o600)
}
