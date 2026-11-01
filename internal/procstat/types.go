// Package procstat samples host process resource usage (CPU, memory, process
// counts) from procfs and attributes it to amux sessions, workgroups and amux's
// own processes.
//
// The daemon runs one Sampler in the background on a fixed interval and serves
// its cached Snapshot through the control API (core.QueryResources); nothing
// here runs on an RPC path. `amux top` and the `resources` sections of
// `amux status --json` render that snapshot, and the TUI resources panel reads
// the same types, so the JSON shape below is a stable, additive contract: add
// fields, never rename or repurpose them.
//
// # Attribution
//
// A process is claimed directly when its cwd or one of its arguments lies under
// a session directory the daemon has stored; that stored owner (session id and
// workgroup) is authoritative. Failing that, the sandbox path's shape is used:
//
//   - <data>/sessions/<wg>/<agent>/...       -> session <agent> in workgroup <wg>
//   - <data>/sessions/<wg>/coordinator/...   -> session <wg> (the coordinator)
//   - <data>/workspaces/<wg>/...             -> session <wg> (workgroup level)
//
// cwd is checked before arguments; argument matching is by substring, so a
// process that merely names a sandbox path (an editor opening an agent's file)
// counts against that agent. Long-lived amux processes (daemon, provide,
// serve, the TUI) are always amux's own (Self), whatever cwd they inherited;
// other amux invocations (an agent's hooks) belong to the sandbox they run in,
// else to Self. Every other process inherits the claim of its nearest claimed
// ancestor, so an agent's compiler, emulator or gradle daemon counts against
// the agent that started it, and the daemon's own git/gh children count as
// amux overhead. A process with no claimed ancestor is Unattributed.
//
// Published command lines (Process.Cmd) have secret-looking flag values,
// key=value pairs, auth headers and well-known token shapes redacted.
//
// # Units
//
// CPUPercent is percent of ONE core over the sample window, summed across a
// group's processes and threads, as top(1) reports it: a busy 4-thread process
// can show 400. RSSBytes sums resident set sizes, so pages shared between a
// group's processes are counted once per process; PSSBytes (proportional set
// size, from smaps_rollup) is reported for individually listed processes only.
package procstat

// Snapshot is one completed sample: the read model behind core.QueryResources.
type Snapshot struct {
	// SampledAt is when the sample completed (Unix ms). Zero means the sampler
	// has not completed a sample yet (or is disabled; see Error).
	SampledAt int64 `json:"sampledAt"`
	// IntervalMS is the sampler's configured period.
	IntervalMS int64 `json:"intervalMs"`
	// WindowMS is the wall-clock span the CPU percentages cover (the time since
	// the previous sample). Zero on the first sample, whose CPU figures are all 0.
	WindowMS int64 `json:"windowMs"`
	// CPUs is the number of online CPUs; 100*CPUs is the host's CPU ceiling.
	CPUs int `json:"cpus"`
	// MemTotalBytes is the host's MemTotal (0 when unreadable).
	MemTotalBytes uint64 `json:"memTotalBytes,omitempty"`

	// Self is amux's own overhead: daemon, provider, TUI and CLI processes and
	// their non-sandbox descendants, plus the sampler's own cost.
	Self SelfUsage `json:"self"`
	// Sessions holds one row per session with at least one attributed process,
	// sorted by CPUPercent descending.
	Sessions []SessionUsage `json:"sessions"`
	// Workgroups rolls Sessions up by WorkgroupID, sorted by CPUPercent
	// descending.
	Workgroups []WorkgroupUsage `json:"workgroups"`
	// Unattributed is every visible process that belongs to neither a session
	// nor amux.
	Unattributed Usage `json:"unattributed"`
	// Total is every visible process.
	Total Usage `json:"total"`

	// Error is set when sampling is unavailable (non-Linux host, no procfs,
	// sampler disabled) or the last sample failed; the other fields then hold
	// the last good sample, if any.
	Error string `json:"error,omitempty"`
	// Unsupported is true when this host cannot be sampled at all (no Linux
	// procfs, e.g. macOS); every figure is then zero. Additive: absent from
	// daemons that predate it.
	Unsupported bool `json:"unsupported,omitempty"`
}

// Usage is an aggregate over a set of processes.
type Usage struct {
	CPUPercent float64 `json:"cpuPercent"` // percent of one core, summed
	RSSBytes   uint64  `json:"rssBytes"`   // summed resident set size
	Processes  int     `json:"processes"`  // process (not thread) count
	Threads    int     `json:"threads"`    // summed thread count
}

// SessionUsage is the usage attributed to one session (an agent, or a
// workgroup's coordinator / workspace-level processes, whose ID is the
// workgroup id itself).
type SessionUsage struct {
	ID          string `json:"id"`          // session id, as in core.Session.ID
	WorkgroupID string `json:"workgroupId"` // the workgroup root id (core.Session.RootID, or ID for a root)
	Usage
	// Top lists the session's heaviest processes (by CPU, then RSS), at most
	// the sampler's TopN.
	Top []Process `json:"top,omitempty"`
}

// WorkgroupUsage sums every SessionUsage with the same WorkgroupID.
type WorkgroupUsage struct {
	ID string `json:"id"`
	Usage
	Sessions int `json:"sessions"` // contributing SessionUsage rows
}

// SelfUsage is amux's own footprint.
type SelfUsage struct {
	// Usage covers amux's processes and their non-sandbox descendants.
	Usage
	// Procs lists every amux process (Role daemon/provide/serve/tui/cli), then
	// up to TopN of their heaviest descendants (Role child), each with its own
	// figures.
	Procs []Process `json:"procs,omitempty"`
	// Sampler is what producing this snapshot cost.
	Sampler SamplerCost `json:"sampler"`
}

// SamplerCost measures the sampler itself.
type SamplerCost struct {
	// CPUMS is the CPU time (user+system) the last sample consumed.
	CPUMS float64 `json:"cpuMs"`
	// WallMS is the last sample's wall-clock duration.
	WallMS float64 `json:"wallMs"`
	// CPUPercent is CPUMS as a share of IntervalMS: the sampler's steady-state
	// cost in percent of one core.
	CPUPercent float64 `json:"cpuPercent"`
	// Scanned is the number of processes the last sample read.
	Scanned int `json:"scanned"`
}

// Process roles (Process.Role) for Self.Procs.
const (
	RoleDaemon  = "daemon"  // amux daemon
	RoleProvide = "provide" // amux provide (provider mode)
	RoleServe   = "serve"   // amux serve (standalone mux server)
	RoleTUI     = "tui"     // the dashboard: bare amux / amux workgroup open
	RoleCLI     = "cli"     // any other amux invocation
	RoleChild   = "child"   // a non-amux descendant of an amux process
)

// Process is one process's figures.
type Process struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Comm string `json:"comm"`          // kernel comm (executable name, ≤15 chars)
	Cmd  string `json:"cmd,omitempty"` // cmdline, space-joined and shortened
	// Role is set on Self.Procs only (Role* constants).
	Role       string  `json:"role,omitempty"`
	CPUPercent float64 `json:"cpuPercent"`
	RSSBytes   uint64  `json:"rssBytes"`
	// PSSBytes is the proportional set size, when smaps_rollup was readable.
	PSSBytes uint64 `json:"pssBytes,omitempty"`
	Threads  int    `json:"threads"`
}
