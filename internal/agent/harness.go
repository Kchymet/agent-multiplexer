package agent

import (
	"log"
	"sort"
	"strings"
	"time"

	"amux/internal/cfghome"
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
//   - launch: Argv, NewSessionID, PlanLaunch, PrepareLaunch
//   - steering: Keys
//   - sandbox: Config (the templated per-agent configuration home)
//   - rail/durability: Activity, RailState, RestoreTranscript
//   - workspace layout: SkillsDir, GuideFile
//   - reasoning-across-sessions: ListSessions
type Harness interface {
	// Kind is the canonical agent kind this harness serves (e.g. "claude").
	Kind() string

	// Models is the selectable model catalog, default first: the harness's
	// built-in list folded with whatever its CLI has cached as available to this
	// account (read live from the user's config home on each call — a cheap file
	// read, so callers may ask freely). May be empty (a harness that takes no
	// --model, or whose models amux doesn't enumerate).
	Models() []string
	// DefaultModel is the first-offered model, or "" when the harness has no
	// enumerated catalog.
	DefaultModel() string
	// PreferredModel is the user's configured model for this harness (read from
	// the CLI's own config), or "" when they haven't set one — callers fall back
	// to DefaultModel.
	PreferredModel() string
	// CurrentModel observes the model the running conversation actually selected.
	// It differs from the stored launch preference after an in-session `/model`.
	CurrentModel(store.Session) (string, bool)

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
	// PrepareLaunch performs the pre-launch filesystem side effects the harness
	// needs for session s launching in dir: trusting the folder (in the agent's
	// own config home) and installing amux's hooks. Best-effort — a failure must
	// never block the launch.
	PrepareLaunch(s store.Session, dir string)

	// Keys are the keystrokes that drive this harness's interactive TUI from
	// outside: submit a line, interrupt a turn, answer a permission prompt. The
	// daemon delivers them to the agent's PTY to serve the published steering
	// verbs (docs/remote-provider-sessions.md §3.1). A zero Keys means the
	// harness cannot be steered by keystroke and the verb is refused.
	Keys() Keys

	// Config describes the agent's private configuration home: the user's own
	// config dir for this harness is the TEMPLATE, a copy of it lives under the
	// agent's sandbox dir, and Spec.Env points the harness at the copy. Nothing of
	// the user's home is mounted into the sandbox except the shared auth file
	// (cfghome.Binds). ok=false for a harness with no config amux templates; such
	// an agent gets no config env and no binds.
	Config(s store.Session) (spec cfghome.Spec, ok bool)

	// Activity reports whether the session is mid-turn (ActivityBusy, unsafe to
	// stop) or idle/between turns (ActivitySafe), or ActivityUnknown when there is
	// no signal — a missing signal never blocks a shutdown. It takes the store
	// session because a harness without a hook stream reads its own transcript
	// state, which lives in the agent's private config home.
	Activity(s store.Session) engine.Activity
	// RailState is the fine-grained state word shown on the rail for a live
	// session (core.State*). Claude reports its hook states directly; harnesses
	// without a hook stream degrade honestly to running/ready via Activity.
	RailState(s store.Session) string
	// RestoreTranscript gap-fills the harness's own transcript for session s under
	// cwd from amux's captured backup, when the harness's copy is missing or
	// staler, so a relaunch resumes the real conversation. Never clobbers a fresher
	// copy; returns whether it restored one.
	RestoreTranscript(s store.Session, cwd string) (bool, error)

	// SkillsDir / GuideFile are the harness's own workspace-config locations under
	// the launch root: where it discovers skills and reads its agent guide. Claude
	// uses .claude/skills + CLAUDE.md; others the vendor-neutral .agents/skills +
	// AGENTS.md.
	SkillsDir(root string) string
	GuideFile(root string) string

	// ListSessions enumerates this harness's on-disk conversations (most recent
	// first) so an agent can reason across sessions: the user's own plus every
	// amux agent's (each has a private home). Empty when the harness keeps no
	// listable transcripts.
	ListSessions() []SessionInfo

	// RuntimeTranscriptPath resolves a stored session to the on-disk transcript
	// path a runtime-event stream tails, and whether one exists. Only harnesses
	// with a supported runtime-event reader return ok=true; others return false so
	// the provider honestly emits nothing rather than advertising a phantom stream.
	RuntimeTranscriptPath(s store.Session) (path string, ok bool)

	// RuntimePermissionPath resolves a stored session to amux's permission journal
	// for it: the record of the permission prompts the runtime itself never writes
	// down (core/permissions.go), read alongside the transcript so a consumer gets
	// `permission_request` events with an id the `permission` verb can answer.
	// ok=false for a harness whose runtime records its own prompts (Codex puts them
	// in the rollout) or that amux does not hook.
	RuntimePermissionPath(s store.Session) (path string, ok bool)

	// Doctor returns human-readable drift/health findings for this harness's
	// integration surface — an empty slice when all is well. `amux doctor` prints
	// them so an upstream CLI change that would silently degrade resume/status/
	// capture surfaces loudly instead. Claude checks its project-dir path munge
	// against Claude's actual on-disk layout; harnesses with no such surface return
	// nothing.
	Doctor() []string
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
	// A single-column update: adopting the codex id must not clobber a concurrent
	// rename/archive from the TUI (which a full-row PutSession of a stale read would).
	_ = db.SetClaudeID(sessionID, id)
}

// mergeModels concatenates model lists into one catalog, keeping first-seen
// order and dropping blanks and duplicates — how a harness folds the models it
// discovers (a cached live catalog, the user's configured pick) into its
// built-in list without offering any twice.
func mergeModels(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, m := range l {
			if m = strings.TrimSpace(m); m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// noopHarness is the Harness for an unrecognized kind: it launches nothing amux
// knows about (Argv errors), reports no activity, and keeps callers free of nil
// checks. A registered kind never resolves to this — only a typo or a legacy
// value nothing can edit does.
type noopHarness struct{ kind string }

func (n noopHarness) Kind() string                            { return n.kind }
func (noopHarness) Models() []string                          { return nil }
func (noopHarness) DefaultModel() string                      { return "" }
func (noopHarness) PreferredModel() string                    { return "" }
func (noopHarness) CurrentModel(store.Session) (string, bool) { return "", false }
func (n noopHarness) Argv(string, ...string) ([]string, error) {
	return nil, &unknownKindError{n.kind}
}
func (noopHarness) NewSessionID() string { return "" }
func (noopHarness) PlanLaunch(req LaunchRequest) LaunchDecision {
	return LaunchDecision{Dir: req.Dir, Extra: freshExtra(req.Prompt)}
}
func (noopHarness) Keys() Keys                                            { return Keys{} }
func (noopHarness) PrepareLaunch(store.Session, string)                   {}
func (noopHarness) Config(store.Session) (cfghome.Spec, bool)             { return cfghome.Spec{}, false }
func (noopHarness) Activity(store.Session) engine.Activity                { return engine.ActivityUnknown }
func (n noopHarness) RailState(s store.Session) string                    { return railStateFromActivity(n.Activity(s)) }
func (noopHarness) RestoreTranscript(store.Session, string) (bool, error) { return false, nil }
func (noopHarness) SkillsDir(root string) string                          { return agentsSkillsDir(root) }
func (noopHarness) GuideFile(root string) string                          { return agentsGuideFile(root) }
func (noopHarness) ListSessions() []SessionInfo                           { return nil }
func (noopHarness) RuntimeTranscriptPath(store.Session) (string, bool)    { return "", false }
func (noopHarness) RuntimePermissionPath(store.Session) (string, bool)    { return "", false }
func (noopHarness) Doctor() []string                                      { return nil }

// unknownKindError is returned by Argv for a kind no registered harness serves.
type unknownKindError struct{ kind string }

func (e *unknownKindError) Error() string { return "unknown agent kind " + `"` + e.kind + `"` }

// agentDirs lists the sandbox dir of every stored agent of kind (plus the
// console's), so a harness can find each agent's private config home — every
// agent's transcripts live in its own home now, not in the user's. Best-effort:
// an unreadable store yields none.
func agentDirs(kind string) []string {
	var out []string
	if consoleDir != nil {
		if d := consoleDir(); d != "" {
			out = append(out, d)
		}
	}
	db, err := store.Open()
	if err != nil {
		return out
	}
	defer db.Close()
	all, err := db.AllSessions()
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, s := range all {
		// A root with a dir is a container session (a workgroup's coordinator or
		// a repo's home) with a private home of its own; a bare root has none. A
		// legacy import records one dir on both the root and its agent — list it once.
		if s.Dir == "" || Canonical(s.Agent) != kind || seen[s.Dir] {
			continue
		}
		seen[s.Dir] = true
		out = append(out, s.Dir)
	}
	return out
}

// consoleDir, when set, returns the built-in console agent's dir so its private
// home is included in listings. The console package registers it (it imports
// this package, so the dependency can't point the other way).
var consoleDir func() string

// RegisterConsoleDir installs the console dir provider (see consoleDir).
func RegisterConsoleDir(f func() string) { consoleDir = f }

// sortSessions orders a merged listing most-recently-modified first.
func sortSessions(s []SessionInfo) {
	sort.Slice(s, func(i, j int) bool { return s[i].Modified.After(s[j].Modified) })
}
