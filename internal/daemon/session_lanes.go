package daemon

import (
	"amux/internal/access"
	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
)

// sessionCallSlow assigns each session call a mailbox lane. Fast calls are
// dispatched in arrival order the moment they are claimed; slow calls run in
// arrival order on the mailbox's slow lane, so a session's status reports,
// launches and reads never queue behind its own creation, restart or
// discovery scan.
//
// Ordering: fast calls stay ordered among themselves (successive activity
// reports, a permission request before its resolution) and slow calls among
// themselves (two actions on one session). A fast and a slow call can finish
// out of order; one client waits for each response before its next call, so
// only concurrent callers in one session, which have no order to keep, can
// observe it. Receipts never reach this classifier: the transport handles
// them inline.
func sessionCallSlow(call sessionrpc.Call) bool {
	req := call.AccessRequest()
	switch {
	case req.Verb == centralauth.QueryVerb, credentialbroker.IsVerb(req.Verb):
		// A launch or a git credential helper is blocked on these, and each is
		// one bounded local operation.
		return false
	case req.Route == access.RouteQuery && sessionreport.IsContextQuery(req.Verb):
		return false
	case req.Route == access.RouteAction && req.Verb == sessionreport.Capture:
		return true // copies up to 256 MiB, and may wait for the capture slot
	case req.Route == access.RouteAction && sessionreport.IsVerb(req.Verb):
		return false // a hook or status line is waiting; each writes one record
	case req.Route == access.RouteQuery:
		switch req.Verb {
		case core.QueryAgentSessions, core.QueryRuntimeEvents:
			return true // scan every transcript or page a large event log
		default:
			return false // store and snapshot projections
		}
	default:
		// Actions create, start, restart, archive and steer sessions, run git
		// and touch worktrees. Unknown verbs are slow too: a slow lane only
		// costs that call its place behind other slow calls.
		return true
	}
}

// sessionCallPriority assigns each session call a response-admission class
// (sessionrpc.ServerOptions.Priority). Self reports and their context query
// are best-effort telemetry: hooks and status lines fire them on every tool
// call and redraw, and a dropped one is superseded by the next. Credential
// reads (a launch or git is blocked on them) and every other action (steer,
// prompt, add-agent, archive) are control and keep reserved capacity.
// Ordinary queries stay normal. Like the lanes, it sees the call before
// authentication and only picks a class: a client mislabeling its own calls
// still competes inside its own session's ceiling.
func sessionCallPriority(call sessionrpc.Call) sessionrpc.Priority {
	req := call.AccessRequest()
	switch {
	case req.Verb == centralauth.QueryVerb, credentialbroker.IsVerb(req.Verb):
		return sessionrpc.PriorityControl
	case req.Route == access.RouteQuery && sessionreport.IsContextQuery(req.Verb):
		return sessionrpc.PriorityTelemetry
	case req.Route == access.RouteAction && sessionreport.IsVerb(req.Verb):
		switch req.Verb {
		case sessionreport.Activity, sessionreport.Model, sessionreport.Limits, sessionreport.Notice:
			return sessionrpc.PriorityTelemetry
		default:
			// A permission prompt someone must answer, or a transcript copy
			// someone may read: not superseded by the next report.
			return sessionrpc.PriorityNormal
		}
	case req.Route == access.RouteAction:
		return sessionrpc.PriorityControl
	default:
		return sessionrpc.PriorityNormal
	}
}
