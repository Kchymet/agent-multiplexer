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
}

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
