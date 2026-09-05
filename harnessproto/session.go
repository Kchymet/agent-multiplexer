package harnessproto

// Session is a normalized agent session surfaced from any Source — the element of
// the v2 "sessions" inventory snapshot (docs/remote-provider-sessions.md §2). It
// is the published subset of amux's internal core.Session (amux aliases core.
// Session to this type), so a provider's inventory frame decodes losslessly in
// any consumer. State ∈ StateIdle|StateReady|StateWaiting|StateRunning|
// StateUnknown (the attention ladder); Section ∈ SectionWorkgroups|SectionRepos|
// SectionDetached|SectionArchived. Archived is emitted only when true.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Source    string `json:"source"`             // claude | hermes | workspace
	Kind      string `json:"kind"`               // agent kind, e.g. claude
	Mode      string `json:"mode,omitempty"`     // task (short) | loop (long)
	RootID    string `json:"rootId,omitempty"`   // parent root for sub-sessions
	IsRoot    bool   `json:"isRoot,omitempty"`   // true => a root container row
	Repos     string `json:"repos,omitempty"`    // agent rows: comma-joined repo names in scope
	Section   string `json:"section,omitempty"`  // rail grouping: workgroups | repos | detached | archived
	State     string `json:"state,omitempty"`    // idle | ready | waiting | running | unknown
	Status    string `json:"status"`             // human label, e.g. "ready · main"
	Archived  bool   `json:"archived,omitempty"` // set on rows in the archived section
	Cwd       string `json:"cwd"`
	Pid       int    `json:"pid,omitempty"`
	StartedAt int64  `json:"startedAt"`
	CanAttach bool   `json:"canAttach"`
	CanKill   bool   `json:"canKill"`
	CanResume bool   `json:"canResume"`

	// Runtime names the runtime backing this session — a value from the Runtime*
	// set (harnessproto/events.go: "claude" | "codex") — so a consumer gates
	// affordances and picks a transcript renderer on the actual runtime rather than
	// assuming one. Additive (AGE-178): a provider that predates this field leaves it
	// empty, and a consumer falls back to Kind (the rail's agent-kind label, which
	// has always carried the same string for agent rows) or a conservative default.
	// It is set only for rows that are a real agent (not repo/workgroup containers).
	Runtime string `json:"runtime,omitempty"`

	// Caps advertises the control verbs the daemon can actually back for THIS
	// session (docs/remote-provider-sessions.md §3.1), correlated to the live
	// runtime — NOT inferred from whether a transcript exists. It is a pointer so a
	// consumer can tell "old provider, capabilities unknown" (nil) from "known, and
	// this verb is off" (a non-nil block with the flag false), and apply a
	// conservative fallback only in the former case. Additive (AGE-178): omitted
	// entirely by a provider that predates it.
	Caps *SessionCaps `json:"caps,omitempty"`

	// ControlMode names HOW this session's steering verbs are delivered and where
	// its runtime events come from — orthogonal to Caps, which says only *which*
	// verbs work (docs/remote-provider-sessions.md §2.2). It is a value from the
	// ControlMode* set (AGE-181):
	//
	//   - ControlModePTY ("pty"): steering is keystroke injection into the agent's
	//     PTY and events are tailed from the runtime's on-disk transcript. This is
	//     the original path, so an empty ControlMode MEANS pty — a consumer must
	//     read "" as ControlModePTY, and a provider predating the field omits it.
	//   - ControlModeStructured ("structured"): steering is a JSON-RPC round-trip to
	//     an amux-supervised runtime App Server, and events are its live structured
	//     stream. Correlated permissions and in-turn interject are native here, not
	//     inferred, and the session has a durable server/thread identity a native
	//     CLI can also attach to.
	//
	// A consumer never sends verbs differently by mode — the session-action route
	// is identical — but it MAY surface a mode-specific affordance (e.g. "attach
	// native CLI") and MAY trust a structured row's correlated permissions without
	// the transcript-heuristic caveat. Additive/omitempty: unset ⇒ pty.
	ControlMode string `json:"controlMode,omitempty"`
}

// SessionCaps is the per-session control surface a provider advertises for a
// published session (docs/remote-provider-sessions.md §3.1). Each flag reports
// whether the daemon's matching steering verb (VerbPrompt / VerbInterject /
// VerbStop / VerbPermission) can actually be served for this session — the honest
// control capability, so a consumer disables an affordance it would only fail on.
//
// Permission is the one that must not be conflated with transcript support: it is
// true only when the runtime raises CORRELATED permission_request events — a
// request_id the VerbPermission verb can quote back and the daemon can match to
// an open prompt (AGE-172) — not merely because the session has a durable
// transcript. A runtime that streams a transcript but cannot correlate an
// answerable approval round-trip reports Permission=false.
type SessionCaps struct {
	Prompt     bool `json:"prompt"`     // deliver a new user turn (may start a stopped agent)
	Interject  bool `json:"interject"`  // deliver text while a turn is already running
	Cancel     bool `json:"cancel"`     // interrupt the in-flight turn without killing the session
	Permission bool `json:"permission"` // answer a correlated permission_request (see the type doc)
}

// Control modes for Session.ControlMode (AGE-181): how a published session's
// steering verbs are delivered and where its runtime events are sourced. The set
// is small and closed; a consumer that meets an unknown value treats the session
// as pty (the conservative, keystroke-driven baseline).
const (
	// ControlModePTY is the original path: steer by keystroke into the agent's PTY,
	// events tailed from the on-disk transcript. An EMPTY ControlMode means this —
	// a provider predating the field omits it and a consumer must read "" as pty.
	ControlModePTY = "pty"
	// ControlModeStructured routes steering as JSON-RPC to an amux-supervised
	// runtime App Server and sources events from its live structured stream, with a
	// durable server/thread identity a native CLI can also attach to.
	ControlModeStructured = "structured"
)

// Agent activity states, surfaced in Session.State. They form an attention
// ladder: a blocked agent (waiting) wants the user more than a working one. This
// is the published enum; amux's internal core re-exports these values and the
// harness orchestrator imports them instead of re-declaring the strings.
const (
	StateIdle    = "idle"    // no live agent process
	StateReady   = "ready"   // live, turn finished, ready for the next message
	StateWaiting = "waiting" // live, blocked on a prompt awaiting user input
	StateRunning = "running" // live and the agent has an active turn
	// StateUnknown is a live agent with no hook data yet (a session predating the
	// hooks, or one that hasn't fired its first event). Shown as a less certain
	// "running" so it reads as live without claiming granular knowledge.
	StateUnknown = "unknown"
)

// Rail sections, top to bottom (Session.Section). The console is sectionless and
// pinned above them all.
const (
	SectionWorkgroups = "workgroups" // cross-repo workgroups + nested agents
	SectionRepos      = "repos"      // tracked repos + their single-repo agents
	SectionDetached   = "detached"   // Claude sessions amux didn't launch
	SectionArchived   = "archived"   // agents marked done/archived (reversible)
)
