package core

import "github.com/kchymet/agent-multiplexer/harnessproto"

// QueryWait blocks until the session named by Action.ID changes — its state,
// its archived flag, or its PRs — or a bounded timeout passes, then returns a
// WaitResult. It is the event-driven alternative to polling `amux agent
// events` or `amux ls` in a loop: the daemon parks the call on its own
// snapshot broadcast and answers the moment the session moves. A restricted
// principal may wait on what it may read (itself; a coordinator its direct
// members).
//
// Fields (both optional):
//   - WaitSinceField: the Token of a previous WaitResult. A session whose
//     current token differs answers at once with Changed; with no token the
//     current view is the baseline. Passing the last token back is how a
//     caller waits again without missing a change between calls.
//   - WaitTimeoutField: whole seconds to wait, 0..MaxWaitSeconds (default
//     MaxWaitSeconds). 0 answers at once with the current view.
const QueryWait = "wait"

// QueryWait fields.
const (
	WaitSinceField   = "since"
	WaitTimeoutField = "timeout"
)

// MaxWaitSeconds bounds one QueryWait call, below the session client's 35 s
// request timeout; a caller that wants to wait longer calls again with the
// returned Token.
const MaxWaitSeconds = 25

// WaitResult is QueryWait's answer: the session's current view and whether it
// changed from the baseline.
type WaitResult struct {
	ID string `json:"id"`
	// Present is false when the session is not on the daemon's rail.
	Present  bool                     `json:"present"`
	State    string                   `json:"state,omitempty"`
	Status   string                   `json:"status,omitempty"`
	Archived bool                     `json:"archived,omitempty"`
	PRs      []harnessproto.SessionPR `json:"prs,omitempty"`
	// Token identifies this view; pass it back as WaitSinceField.
	Token string `json:"token"`
	// Changed is true when the view differs from the baseline; false when the
	// call timed out (or asked for timeout 0 with no baseline).
	Changed bool `json:"changed"`
}
