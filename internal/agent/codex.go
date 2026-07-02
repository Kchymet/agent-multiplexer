package agent

import (
	"os"
	"time"

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

// PlanLaunch decides how to start a Codex agent — resume an existing rollout or
// start fresh — and keeps the pinned rollout uuid in step with what's on disk. A
// pinned id resumes iff its rollout file still exists (`codex resume <id>` locates
// a session by uuid regardless of cwd, so existence is the exact question). With
// no usable pin it adopts the newest rollout recorded under the launch dir,
// persisting it so later launches resume it.
func (codexHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	s := req.Session
	if s.ClaudeID != "" {
		if _, ok := codexcfg.RolloutPath(s.ClaudeID); ok {
			core.ClearNotice(s.ClaudeID)
			return LaunchDecision{Dir: req.Dir, Extra: []string{"resume", s.ClaudeID}}
		}
		warnResumeFailed(req)
	}
	// No usable pin. Adopt the newest rollout under this dir if there is one. When
	// this replaces a lost pin, key the rail notice under the adopted id: the rail
	// reads notices by the pinned id, so one left under the lost id would never render.
	if id, ok := codexcfg.LatestSession(req.Dir); ok {
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

// PrepareLaunchDir pre-trusts the launch dir so Codex doesn't prompt to trust the
// folder on startup. Codex has no hook mechanism to install.
func (codexHarness) PrepareLaunchDir(dir string) { _ = codexcfg.TrustDir(dir) }

// AgentConfigBinds binds the whole $CODEX_HOME tree writable — Codex keeps auth
// (auth.json), config (config.toml), and its rollout transcripts there and writes
// rollouts mid-session. It lives under the tmpfs'd $HOME, so create it first or
// the writes land on ephemeral tmpfs.
func (codexHarness) AgentConfigBinds(string) [][]string {
	ch := codexcfg.Home()
	_ = os.MkdirAll(ch, 0o755)
	return [][]string{{"--bind-try", ch, ch}}
}

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
func (codexHarness) Activity(sessionID string) engine.Activity {
	if rec, ok := core.HookState(sessionID); ok {
		switch rec.State {
		case core.StateRunning, core.StateWaiting:
			return engine.ActivityBusy
		case core.StateReady, core.StateIdle:
			return engine.ActivitySafe
		}
	}
	path, ok := codexcfg.RolloutPath(sessionID)
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
func (h codexHarness) RailState(sessionID string) string {
	return railStateFromActivity(h.Activity(sessionID))
}

// RestoreTranscript gap-fills Codex's rollout for sessionID from amux's captured
// backup. When a rollout already exists it restores into that path (the one
// `codex resume` reads); when none does, it reconstructs a plausible rollout path
// so a subsequent `codex resume <id>` can still discover the gap-filled
// transcript. cwd is unused — Codex keys rollouts by uuid, not by munged cwd.
func (codexHarness) RestoreTranscript(cwd, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	dst, ok := codexcfg.RolloutPath(sessionID)
	if !ok {
		dst = codexcfg.NewRolloutPath(sessionID)
	}
	return core.RestoreCapturedTranscript(sessionID, dst)
}

// SkillsDir / GuideFile: Codex reads the vendor-neutral Agent Skills layout —
// .agents/skills and AGENTS.md under the launch root.
func (codexHarness) SkillsDir(root string) string { return agentsSkillsDir(root) }
func (codexHarness) GuideFile(root string) string { return agentsGuideFile(root) }

// ListSessions maps Codex's rollout listing into the kind-neutral shape.
func (codexHarness) ListSessions() []SessionInfo {
	src := codexcfg.ListSessions()
	out := make([]SessionInfo, 0, len(src))
	for _, s := range src {
		out = append(out, SessionInfo{
			ID: s.ID, Cwd: s.Cwd, Project: s.Project, Path: s.Path, Size: s.Size, Modified: s.Modified,
		})
	}
	return out
}

// RuntimeTranscriptPath returns false: Codex's rollout format has no supported
// runtime-event reader yet, so the provider honestly emits nothing for a Codex
// session rather than advertising a phantom (Claude-shaped) stream.
func (codexHarness) RuntimeTranscriptPath(store.Session) (string, bool) { return "", false }
