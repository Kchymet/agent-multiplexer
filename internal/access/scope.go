package access

import (
	"strings"

	"amux/internal/core"
)

// Scope is the authorization scope a restricted session principal needs for one
// daemon control-API operation. The host principal is never restricted.
type Scope string

const (
	// ScopeHost operations are reserved for the authenticated host. The console
	// may hold one as an explicit delegated operator grant (Rule.Console).
	ScopeHost Scope = "host"
	// ScopeSession operations name no session resource and return no per-session
	// data beyond what any active session may see (diagnostics, discovery).
	ScopeSession Scope = "session"
	// ScopeFiltered list operations are allowed for every active session; the
	// daemon projects their result down to the caller's InWorkgroupScope set
	// instead of refusing.
	ScopeFiltered Scope = "filtered"
	// ScopeWorkgroup operations must target a session InWorkgroupScope: the
	// caller itself (self scope, for ordinary agents and repo homes), or for a
	// coordinator also a current direct member of its workgroup.
	ScopeWorkgroup Scope = "workgroup"
)

// Rule classifies one route/verb pair.
type Rule struct {
	Scope Scope
	// Console marks an operation the operator console holds as a delegated grant
	// on any target. Every other restricted role is still held to Scope.
	Console bool
	// Allow further narrows a workgroup rule once the target is known to be
	// in scope. self reports whether the target is the caller.
	Allow func(req Request, self bool, target Resource) bool
}

// rules is the complete restricted control-API vocabulary. Every core Action*
// and Query* constant must appear here (TestEveryCoreOperationIsClassified);
// anything absent is denied to every restricted principal.
var rules = map[Route]map[string]Rule{
	RouteAction: {
		core.ActionStart:       {Scope: ScopeWorkgroup},
		core.ActionSteer:       {Scope: ScopeWorkgroup, Allow: allowSteer},
		core.ActionRename:      {Scope: ScopeWorkgroup},
		core.ActionSetArchived: {Scope: ScopeWorkgroup, Console: true, Allow: allowArchive},
		core.ActionAddAgent:    {Scope: ScopeWorkgroup, Console: true, Allow: allowAddAgent},

		core.ActionRefresh:         {Scope: ScopeHost, Console: true},
		core.ActionMove:            {Scope: ScopeHost, Console: true},
		core.ActionDelete:          {Scope: ScopeHost, Console: true},
		core.ActionKill:            {Scope: ScopeHost, Console: true},
		core.ActionAddRepo:         {Scope: ScopeHost, Console: true},
		core.ActionRmRepo:          {Scope: ScopeHost, Console: true},
		core.ActionAgentSetRepos:   {Scope: ScopeHost, Console: true},
		core.ActionNewWorkgroup:    {Scope: ScopeHost, Console: true}, // repo homes: own repo only
		core.ActionCreateWorkspace: {Scope: ScopeHost, Console: true},

		core.ActionArchive:             {Scope: ScopeHost}, // toggle can restore
		core.ActionRestart:             {Scope: ScopeHost},
		core.ActionAuthReload:          {Scope: ScopeHost},
		core.ActionAccountManage:       {Scope: ScopeHost},
		core.ActionCodexLogin:          {Scope: ScopeHost},
		core.ActionCoordinatorSetRepos: {Scope: ScopeHost},
		core.ActionSubscribe:           {Scope: ScopeHost},
		core.ActionQuery:               {Scope: ScopeHost}, // restricted reads use RouteQuery
	},
	RoutePane: {
		// Restricted principals use one-shot mailbox RPC only.
		core.ActionPaneOpen:   {Scope: ScopeHost},
		core.ActionPaneInput:  {Scope: ScopeHost},
		core.ActionPaneResize: {Scope: ScopeHost},
		core.ActionPaneClose:  {Scope: ScopeHost},
	},
	RouteQuery: {
		core.QueryVersion:       {Scope: ScopeSession},
		core.QueryCodexControl:  {Scope: ScopeSession},
		core.QueryAgentSessions: {Scope: ScopeSession},

		core.QuerySessions: {Scope: ScopeFiltered},
		core.QuerySnapshot: {Scope: ScopeFiltered},
		core.QueryRepos:    {Scope: ScopeFiltered},

		core.QueryRuntimeEvents: {Scope: ScopeWorkgroup},

		// Raw runtime paths/records and untracked-conversation fallback are never
		// exposed to restricted principals.
		core.QueryRuntimePath:     {Scope: ScopeHost},
		core.QueryRuntimeRecord:   {Scope: ScopeHost},
		core.QueryAccounts:        {Scope: ScopeHost},
		core.QueryAccountManage:   {Scope: ScopeHost},
		core.QueryAccountCeremony: {Scope: ScopeHost},
		core.QueryCodexAuth:       {Scope: ScopeHost},
	},
}

// RuleFor returns the classification of one route/verb pair; ok is false for an
// unclassified operation, which Authorize denies.
func RuleFor(route Route, verb string) (Rule, bool) {
	rule, ok := rules[route][verb]
	return rule, ok
}

// Classified lists every classified verb per route, for tests and docs.
func Classified() map[Route]map[string]Scope {
	out := make(map[Route]map[string]Scope, len(rules))
	for route, verbs := range rules {
		out[route] = make(map[string]Scope, len(verbs))
		for verb, rule := range verbs {
			out[route][verb] = rule.Scope
		}
	}
	return out
}

// allowSteer permits prompt, interject, and stop in scope. A permission answer
// must name its request and runtime generation, and is never accepted for the
// caller's own prompt: a session approving itself would bypass the approval.
func allowSteer(req Request, self bool, _ Resource) bool {
	switch req.Fields[core.SteerVerb] {
	case core.SteerPrompt, core.SteerInterject, core.SteerStop:
		return true
	case core.SteerPermission:
		return !self && strings.TrimSpace(req.Fields[core.SteerRequestID]) != "" &&
			strings.TrimSpace(req.Fields[RuntimeGenerationField]) != ""
	}
	return false
}

// allowArchive permits explicit archival: an ordinary agent completing itself,
// a coordinator archiving a direct member, or a coordinator archiving its own
// workgroup (which cascades to its members; guides require explicit user
// confirmation first). Repo homes and other roots never archive themselves,
// and every restore is host-only.
//
// Forcing past the open pull-request guard (force=true) is narrower: only a
// coordinator archiving a direct member may. An agent can never force its own
// completion, nor a coordinator its whole workgroup — that takes the host.
func allowArchive(req Request, self bool, target Resource) bool {
	if req.Fields["archived"] != "true" {
		return false
	}
	if req.Fields["force"] == "true" && self {
		return false
	}
	return target.RootID != "" || (self && target.Role == "coordinator")
}

// allowAddAgent permits a coordinator to add to its own workgroup only; its
// field and repo-grant checks run in Policy.authorizeAddAgent.
func allowAddAgent(_ Request, self bool, target Resource) bool {
	return self && target.Role == "coordinator"
}
