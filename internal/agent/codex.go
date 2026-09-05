package agent

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"amux/internal/cfghome"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/store"
)

// codexHarness implements Harness for OpenAI's Codex CLI, mapping its on-disk
// rollout convention onto the abstract primitives. Codex has no hook mechanism to
// report turn state, so Activity falls back to rollout-file freshness, and it owns
// its own session-id lifecycle (it mints its own uuid — amux can't pin one).
type codexHarness struct{}

func (codexHarness) Kind() string { return "codex" }

// codexModels is the selectable Codex model catalog. gpt-5.5 is the recommended
// default and comes first.
var codexModels = []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark"}

func (codexHarness) Models() []string       { return codexModels }
func (codexHarness) DefaultModel() string   { return codexModels[0] }
func (codexHarness) PreferredModel() string { return codexcfg.PreferredModel() }

// Argv builds Codex's launch argv. It defaults to autonomous operation, mirroring
// claude's permission-mode convention: a sandbox unless the user opts out with
// AMUX_CODEX_SANDBOX=none. Override the level with AMUX_CODEX_SANDBOX=
// read-only|workspace-write|danger-full-access.
func (codexHarness) Argv(model string, extra ...string) ([]string, error) {
	bin := envOr("AMUX_CODEX_BIN", "codex")
	var args []string
	if sb := envOr("AMUX_CODEX_SANDBOX", "workspace-write"); sb != "none" {
		args = append(args, "--sandbox", sb)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	return finishArgv(bin, args, extra), nil
}

// NewSessionID returns "" — Codex mints its own uuid on its first run and can't be
// told one up front; amux adopts the real id afterward (see PlanLaunch).
func (codexHarness) NewSessionID() string { return "" }

// Config templates the user's Codex home into the agent's sandbox: the copy
// lives at <agent dir>/.amux/codex and CODEX_HOME points Codex at it (see
// codexcfg.Template).
func (codexHarness) Config(s store.Session) (cfghome.Spec, bool) {
	if s.Dir == "" {
		return cfghome.Spec{}, false
	}
	return codexcfg.Template(s.ID, codexcfg.AgentHome(s.Dir)), true
}

// home is the agent's private Codex home (its rollouts, config, trust), or the
// user's when the session has no dir.
func (h codexHarness) home(s store.Session) codexcfg.Home {
	if sp, ok := h.Config(s); ok {
		return codexcfg.At(sp.Dir)
	}
	return codexcfg.UserHome()
}

// PlanLaunch decides how to start a Codex agent — resume an existing rollout or
// start fresh — and keeps the pinned rollout uuid in step with what's on disk. A
// pinned id resumes iff its rollout file still exists (`codex resume <id>` locates
// a session by uuid regardless of cwd, so existence is the exact question). With
// no usable pin it adopts the newest rollout recorded under the launch dir,
// persisting it so later launches resume it.
//
// Rollouts live in the agent's private home; one recorded in the user's home
// before homes were private is carried over on first sight.
func (h codexHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	s := req.Session
	home := h.home(s)
	if s.ClaudeID != "" {
		carryOverRollout(home, s.ClaudeID)
		if _, ok := home.RolloutPath(s.ClaudeID); ok {
			core.ClearNotice(s.ClaudeID)
			return LaunchDecision{Dir: req.Dir, Extra: []string{"resume", s.ClaudeID}}
		}
		warnResumeFailed(req)
	}
	// No usable pin. Adopt the newest rollout under this dir if there is one. When
	// this replaces a lost pin, key the rail notice under the adopted id: the rail
	// reads notices by the pinned id, so one left under the lost id would never render.
	// A rollout the user's home recorded for this dir before homes were private is
	// carried over first, so the adoption sees it.
	if id, ok := codexcfg.UserHome().LatestSession(req.Dir); ok {
		carryOverRollout(home, id)
	}
	if id, ok := home.LatestSession(req.Dir); ok {
		if s.ClaudeID != "" && id != s.ClaudeID {
			_ = core.WriteNotice(id, "couldn't resume pinned conversation — resumed the newest one instead")
		}
		persistConvID(s.ID, id)
		return LaunchDecision{Dir: req.Dir, Extra: []string{"resume", id}}
	}
	if s.ClaudeID != "" {
		persistConvID(s.ID, "")
	}
	return LaunchDecision{Dir: req.Dir, Extra: freshExtra(req.Prompt)}
}

// carryOverRollout copies uuid's rollout from the user's home into home when
// home lacks it and the user's has it — the one-time migration of a
// conversation recorded before the agent had a private home.
func carryOverRollout(home codexcfg.Home, uuid string) {
	user := codexcfg.UserHome()
	if home == user {
		return
	}
	if _, ok := home.RolloutPath(uuid); ok {
		return
	}
	src, ok := user.RolloutPath(uuid)
	if !ok {
		return
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return
	}
	dst := home.NewRolloutPath(uuid)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		log.Printf("amux: carrying rollout %s into the agent's config home: %v", uuid, err)
		return
	}
	log.Printf("amux: carried rollout %s into the agent's private Codex home", uuid)
}

// PrepareLaunch pre-trusts the launch dir in the agent's own home so Codex
// doesn't prompt to trust the folder on startup. Codex has no hook mechanism to
// install.
func (h codexHarness) PrepareLaunch(s store.Session, dir string) { _ = h.home(s).TrustDir(dir) }

// Keys are Codex's interactive bindings (see codexKeys).
func (codexHarness) Keys() Keys { return codexKeys() }

// codexBusyWindow is how recently a rollout must have been written for Activity to
// treat the session as mid-turn — a heuristic tuned to err toward "busy" only
// briefly after the last write so a graceful stop waits out an active turn without
// being blocked forever by a stale file.
const codexBusyWindow = 45 * time.Second

// Activity reports Codex's turn state. Codex exposes no hook signal, so this is
// two-tier: first honor an explicit state if something recorded one in the same
// store Claude's hooks use; otherwise fall back to the rollout file's mtime —
// written within codexBusyWindow reads as Busy, an older rollout as Safe, and no
// rollout at all as Unknown (never blocks a shutdown).
func (h codexHarness) Activity(s store.Session) engine.Activity {
	if rec, ok := core.HookState(s.ClaudeID); ok {
		switch rec.State {
		case core.StateRunning, core.StateWaiting:
			return engine.ActivityBusy
		case core.StateReady, core.StateIdle:
			return engine.ActivitySafe
		}
	}
	path, ok := h.home(s).RolloutPath(s.ClaudeID)
	if !ok {
		return engine.ActivityUnknown
	}
	fi, err := os.Stat(path)
	if err != nil {
		return engine.ActivityUnknown
	}
	if time.Since(fi.ModTime()) < codexBusyWindow {
		return engine.ActivityBusy
	}
	return engine.ActivitySafe
}

// RailState degrades honestly to running/ready via the coarse Activity signal —
// Codex can't report Claude's fine-grained turn states.
func (h codexHarness) RailState(s store.Session) string {
	return railStateFromActivity(h.Activity(s))
}

// RestoreTranscript gap-fills Codex's rollout for the session from amux's
// captured backup. When a rollout already exists it restores into that path (the
// one `codex resume` reads); when none does, it reconstructs a plausible rollout
// path in the agent's home so a subsequent `codex resume <id>` can still discover
// the gap-filled transcript. cwd is unused — Codex keys rollouts by uuid, not by
// munged cwd.
func (h codexHarness) RestoreTranscript(s store.Session, cwd string) (bool, error) {
	if s.ClaudeID == "" {
		return false, nil
	}
	home := h.home(s)
	dst, ok := home.RolloutPath(s.ClaudeID)
	if !ok {
		dst = home.NewRolloutPath(s.ClaudeID)
	}
	return core.RestoreCapturedTranscript(s.ClaudeID, dst)
}

// SkillsDir / GuideFile: Codex reads the vendor-neutral Agent Skills layout —
// .agents/skills and AGENTS.md under the launch root.
func (codexHarness) SkillsDir(root string) string { return agentsSkillsDir(root) }
func (codexHarness) GuideFile(root string) string { return agentsGuideFile(root) }

// homes is every Codex home on the machine: the user's own, then each agent's.
func (h codexHarness) homes() []codexcfg.Home {
	out := []codexcfg.Home{codexcfg.UserHome()}
	for _, d := range agentDirs(h.Kind()) {
		out = append(out, codexcfg.At(codexcfg.AgentHome(d)))
	}
	return out
}

// ListSessions merges every Codex home's rollout listing into the kind-neutral
// shape, most recent first.
func (h codexHarness) ListSessions() []SessionInfo {
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

// RuntimeTranscriptPath resolves a Codex session to its rollout jsonl — the
// record internal/runtimeevents tails for structured events. Codex keys rollouts
// by uuid rather than by cwd, so the pinned id is the whole lookup within the
// agent's home; a session whose rollout is not on disk (never launched, or
// pruned) resolves to false and the provider honestly emits nothing for it.
func (h codexHarness) RuntimeTranscriptPath(s store.Session) (string, bool) {
	if s.ClaudeID == "" {
		return "", false
	}
	if p, ok := h.home(s).RolloutPath(s.ClaudeID); ok {
		return p, true
	}
	// Not yet carried into the agent's home (no launch since homes became
	// private): answer the user's copy so a live reader keeps following it.
	if user := codexcfg.UserHome(); user != h.home(s) {
		return user.RolloutPath(s.ClaudeID)
	}
	return "", false
}

// RuntimePermissionPath reports that Codex has no separate permission journal:
// Codex records its approval prompts in the rollout itself, so the transcript
// reader already produces permission_request events for them and a second source
// would only duplicate them.
func (codexHarness) RuntimePermissionPath(store.Session) (string, bool) { return "", false }

// Doctor: Codex has no amux-managed config surface to drift-check.
func (codexHarness) Doctor() []string { return nil }
