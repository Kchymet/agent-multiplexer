package agent

import (
	"log"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/store"
)

// Harness abstracts one agent CLI so every kind-specific decision lives behind a
// single value instead of being smeared across the packages that need it. amux's
// durability machinery (turn-aware shutdown, transcript gap-fill), its launcher,
// its sandbox, and its rail all consult the Harness rather than switching on the
// kind string themselves — HarnessFor is the one and only kind switch.
//
// The method set is grouped by concern:
//
//   - identity & catalog: Kind, Models, DefaultModel, PreferredModel
//   - launch: Argv, NewSessionID, PlanLaunch, PrepareLaunchDir
//   - sandbox: AgentConfigBinds
//   - rail/durability: Activity, RailState, RestoreTranscript
//   - workspace layout: SkillsDir, GuideFile
//   - reasoning-across-sessions: ListSessions
type Harness interface {
	// Kind is the canonical agent kind this harness serves (e.g. "claude").
	Kind() string

	// Models is the selectable model catalog, default first. May be empty (a
	// harness that takes no --model, or whose models amux doesn't enumerate).
	Models() []string
	// DefaultModel is the built-in default (first-offered) model, or "" when the
	// harness has no enumerated catalog.
	DefaultModel() string
	// PreferredModel is the user's configured model for this harness (read from
	// the CLI's own config), or "" when they haven't set one — callers fall back
	// to DefaultModel.
	PreferredModel() string

	// Argv builds the absolute launch argv (with an optional model override and
	// trailing args). The launch binary is overridable via AMUX_<KIND>_BIN.
	Argv(model string, extra ...string) ([]string, error)
	// NewSessionID returns a conversation id to pin at creation (Claude accepts a
	// pre-minted --session-id), or "" when the harness mints its own on first run
	// (Codex) — in which case amux adopts the real id after launch.
	NewSessionID() string
	// PlanLaunch decides resume vs continue vs fresh for a session and returns the
	// launch dir (which may move to where a transcript already lives) and the
	// trailing args to pass before Argv's own. It may persist an adopted session
	// id as a side effect (Codex).
	PlanLaunch(req LaunchRequest) LaunchDecision
	// PrepareLaunchDir performs the pre-launch filesystem side effects the harness
	// needs in its launch dir: trusting the folder and installing amux's hooks.
	// Best-effort — a failure must never block the launch.
	PrepareLaunchDir(dir string)

	// AgentConfigBinds returns the bubblewrap binds specific to this harness's
	// agent pane (its config/auth/state under $HOME), on top of the shared binds
	// the sandbox always adds. home is the user's home dir.
	AgentConfigBinds(home string) [][]string

	// Activity reports whether the instance is mid-turn (ActivityBusy, unsafe to
	// stop) or idle/between turns (ActivitySafe), or ActivityUnknown when there is
	// no signal — a missing signal never blocks a shutdown.
	Activity(sessionID string) engine.Activity
	// RailState is the fine-grained state word shown on the rail for a live
	// session (core.State*). Claude reports its hook states directly; harnesses
	// without a hook stream degrade honestly to running/ready via Activity.
	RailState(sessionID string) string
	// RestoreTranscript gap-fills the harness's own transcript for sessionID under
	// cwd from amux's captured backup, when the harness's copy is missing or
	// staler, so a relaunch resumes the real conversation. Never clobbers a fresher
	// copy; returns whether it restored one.
	RestoreTranscript(cwd, sessionID string) (bool, error)

	// SkillsDir / GuideFile are the harness's own workspace-config locations under
	// the launch root: where it discovers skills and reads its agent guide. Claude
	// uses .claude/skills + CLAUDE.md; others the vendor-neutral .agents/skills +
	// AGENTS.md.
	SkillsDir(root string) string
	GuideFile(root string) string

	// ListSessions enumerates this harness's on-disk conversations (most recent
	// first) so an agent can reason across sessions. Empty when the harness keeps
	// no listable transcripts.
	ListSessions() []SessionInfo

	// RuntimeTranscriptPath resolves a stored session to the on-disk transcript
	// path a runtime-event stream tails, and whether one exists. Only harnesses
	// with a supported runtime-event reader return ok=true; others return false so
	// the provider honestly emits nothing rather than advertising a phantom stream.
	RuntimeTranscriptPath(s store.Session) (path string, ok bool)
}

// SessionInfo is one of a harness's on-disk conversations, in a kind-neutral
// shape so `amux agent sessions` can merge every harness's listing uniformly.
type SessionInfo struct {
	ID       string
	Cwd      string
	Project  string
	Path     string
	Size     int64
	Modified time.Time
}

// LaunchRequest carries everything a harness's PlanLaunch needs without coupling
// it to how the caller derived the values.
type LaunchRequest struct {
	Session store.Session // the session being launched
	Dir     string        // the stat-verified launch dir (the workspace root)
	Prompt  string        // the trimmed initial prompt
	// ResumeCwds are the candidate cwds a transcript for this session could live
	// under (amux's workdir convention has shifted over time), preferred-first.
	ResumeCwds []string
}

// LaunchDecision is a PlanLaunch result: the (possibly relocated) launch dir and
// the trailing args to pass ahead of the harness's own Argv args.
type LaunchDecision struct {
	Dir   string
	Extra []string
}

// registry is the ordered set of built-in harnesses, default (claude) first.
// Adding a kind is a single new harness file plus one entry here.
var registry = []Harness{claudeHarness{}, codexHarness{}, hermesHarness{}}

// HarnessFor returns the Harness for an agent kind. "" resolves to the default
// (first-registered) harness; a registered kind returns its harness; any other
// kind gets a no-op harness so an unrecognized kind still launches via Argv but
// opts out of durability signals rather than nil-panicking a caller.
func HarnessFor(kind string) Harness {
	if kind == "" {
		return registry[0]
	}
	for _, h := range registry {
		if h.Kind() == kind {
			return h
		}
	}
	return noopHarness{kind: kind}
}

// Harnesses returns the registered harnesses in order (default first) — the
// single source for iterating every kind (session listings, doctor, UI pickers).
func Harnesses() []Harness { return registry }

// Kinds returns the registered kinds in order (default first), for UI pickers and
// the harness selector.
func Kinds() []string {
	out := make([]string, len(registry))
	for i, h := range registry {
		out[i] = h.Kind()
	}
	return out
}

// DefaultKind is the kind amux uses when none is chosen — the first-registered
// harness ("claude"). Callers use this instead of hardcoding the literal.
func DefaultKind() string { return registry[0].Kind() }

// Canonical maps a possibly-empty kind to its canonical registered form (""
// becomes the default kind), so callers persist/compare a single spelling.
func Canonical(kind string) string { return HarnessFor(kind).Kind() }

// Known reports whether kind names a launchable harness. Creation paths check it
// before persisting a session so a mistyped kind (which nothing can edit after
// the fact) is rejected up front instead of minting a session that errors on
// every launch. "" is known (it resolves to the default).
func Known(kind string) bool {
	if kind == "" {
		return true
	}
	for _, h := range registry {
		if h.Kind() == kind {
			return true
		}
	}
	return false
}

// railStateFromActivity maps a coarse Activity signal onto a rail state word,
// for harnesses without a fine-grained hook stream: live-and-busy reads as
// running, live-and-idle as ready, no-signal as unknown (still live).
func railStateFromActivity(a engine.Activity) string {
	switch a {
	case engine.ActivityBusy:
		return core.StateRunning
	case engine.ActivitySafe:
		return core.StateReady
	default:
		return core.StateUnknown
	}
}

// freshExtra is the trailing args for a fresh interactive run: the prompt as a
// positional, or nothing when there's no prompt.
func freshExtra(prompt string) []string {
	if prompt != "" {
		return []string{prompt}
	}
	return nil
}

// warnResumeFailed surfaces — in the daemon log and on the rail — that a pinned
// conversation couldn't be resumed, so the user knows they've been dropped into a
// fresh session. The rail notice is keyed by the conversation id and cleared the
// next time the session resumes successfully.
func warnResumeFailed(req LaunchRequest) {
	s := req.Session
	log.Printf("amux: pinned %s conversation %s (agent %s) has no transcript under %v; starting a new conversation",
		Canonical(s.Agent), s.ClaudeID, s.ID, req.ResumeCwds)
	_ = core.WriteNotice(s.ClaudeID, "couldn't resume pinned conversation — started fresh")
}

// persistConvID records id (possibly "") as the session's pinned conversation id.
// Codex adopts the uuid it minted only after its first run, and amux clears it
// when the named rollout disappears, so this write keeps the store in step with
// what `codex resume` will actually find. It re-reads the record so a concurrent
// change to other fields isn't clobbered. Best-effort.
func persistConvID(sessionID, id string) {
	db, err := store.Open()
	if err != nil {
		return
	}
	defer db.Close()
	cur, ok, err := db.GetSession(sessionID)
	if err != nil || !ok {
		return
	}
	cur.ClaudeID = id
	_ = db.PutSession(cur)
}

// noopHarness is the Harness for an unrecognized kind: it launches nothing amux
// knows about (Argv errors), reports no activity, and keeps callers free of nil
// checks. A registered kind never resolves to this — only a typo or a legacy
// value nothing can edit does.
type noopHarness struct{ kind string }

func (n noopHarness) Kind() string         { return n.kind }
func (noopHarness) Models() []string       { return nil }
func (noopHarness) DefaultModel() string   { return "" }
func (noopHarness) PreferredModel() string { return "" }
func (n noopHarness) Argv(string, ...string) ([]string, error) {
	return nil, &unknownKindError{n.kind}
}
func (noopHarness) NewSessionID() string { return "" }
func (noopHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	return LaunchDecision{Dir: req.Dir, Extra: freshExtra(req.Prompt)}
}
func (noopHarness) PrepareLaunchDir(string)                            {}
func (noopHarness) AgentConfigBinds(string) [][]string                 { return nil }
func (noopHarness) Activity(string) engine.Activity                    { return engine.ActivityUnknown }
func (n noopHarness) RailState(sid string) string                      { return railStateFromActivity(n.Activity(sid)) }
func (noopHarness) RestoreTranscript(string, string) (bool, error)     { return false, nil }
func (noopHarness) SkillsDir(root string) string                       { return agentsSkillsDir(root) }
func (noopHarness) GuideFile(root string) string                       { return agentsGuideFile(root) }
func (noopHarness) ListSessions() []SessionInfo                        { return nil }
func (noopHarness) RuntimeTranscriptPath(store.Session) (string, bool) { return "", false }

// unknownKindError is returned by Argv for a kind no registered harness serves.
type unknownKindError struct{ kind string }

func (e *unknownKindError) Error() string { return "unknown agent kind " + `"` + e.kind + `"` }
