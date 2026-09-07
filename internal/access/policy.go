package access

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"amux/internal/core"
)

var ErrDenied = errors.New("access denied")

type Route string

const (
	RouteQuery  Route = "query"
	RouteAction Route = "action"
	RoutePane   Route = "pane"
)

type Request struct {
	Route  Route
	Verb   string
	ID     string
	Target string
	Tab    int
	Fields map[string]string
}

// Resource is the authorization-only projection of a session. Resolver must
// derive it from daemon-owned state; clients never populate these fields.
type Resource struct {
	ID, RootID, Role, Scope, Repo string
	Archived                      bool
}

type Resolver interface {
	Lookup(context.Context, string) (Resource, bool, error)
	RepoGranted(context.Context, string, string) (bool, error)
	ProviderAllows(context.Context, string, Request) (bool, error)
}

type Policy struct{ Resolver Resolver }

func (p Policy) Authorize(ctx context.Context, principal Principal, req Request) error {
	if principal.Kind == SubjectHost {
		return nil
	}
	if principal.Kind == SubjectProvider {
		if p.Resolver == nil {
			return ErrDenied
		}
		ok, err := p.Resolver.ProviderAllows(ctx, principal.SubjectID, req)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		return ErrDenied
	}
	if principal.Kind != SubjectSession || p.Resolver == nil {
		return ErrDenied
	}
	subject, ok, err := p.Resolver.Lookup(ctx, principal.SubjectID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrDenied
	}
	if subject.Archived {
		return ErrDenied
	}

	if req.Route == RouteQuery {
		return authorizeQuery(subject, req)
	}
	if req.Route == RoutePane {
		return ErrDenied // restricted principals use one-shot mailbox RPC only
	}
	if req.Route != RouteAction {
		return ErrDenied
	}

	target, targetOK, err := p.Resolver.Lookup(ctx, req.ID)
	if err != nil {
		return err
	}
	switch subject.Role {
	case "console":
		if consoleAction(req.Verb) {
			return nil
		}
	case "coordinator":
		if req.Verb == core.ActionAddAgent && req.ID == subject.ID {
			if err := p.authorizeAddAgent(ctx, subject, req); err == nil {
				return nil
			}
		}
		if targetOK && (target.ID == subject.ID || target.RootID == subject.ID) && coordinatorAction(req, target.ID == subject.ID) {
			return nil
		}
	case "repo":
		if req.Verb == core.ActionNewRepoAgent && req.ID == subject.Repo {
			return nil
		}
		owned, err := p.repoOwns(ctx, subject, target)
		if err != nil {
			return err
		}
		if targetOK && owned && coordinatorAction(req, target.ID == subject.ID) {
			return nil
		}
	case "": // ordinary agent: explicit self-only actions
		if targetOK && target.ID == subject.ID && ordinaryAction(req) {
			return nil
		}
	default:
		return ErrDenied
	}
	return ErrDenied
}

func (p Policy) authorizeAddAgent(ctx context.Context, subject Resource, req Request) error {
	allowedFields := map[string]bool{"agent": true, "prompt": true, "mode": true, "model": true, "repos": true}
	for key := range req.Fields {
		if !allowedFields[key] {
			return ErrDenied
		}
	}
	for _, repo := range splitList(req.Fields["repos"]) {
		ok, err := p.Resolver.RepoGranted(ctx, subject.ID, repo)
		if err != nil {
			return err
		}
		if !ok {
			return ErrDenied
		}
	}
	return nil
}

func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// repoOwns follows the target to its authoritative repo-scoped hidden root.
// Merely sharing target.Repo is insufficient: an unrelated workgroup member can
// be assigned the same repository.
func (p Policy) repoOwns(ctx context.Context, subject, target Resource) (bool, error) {
	if target.ID == subject.ID {
		return true, nil
	}
	if target.RootID == "" {
		return target.Role == "" && target.Scope == "repo" && target.Repo == subject.Repo, nil
	}
	root, ok, err := p.Resolver.Lookup(ctx, target.RootID)
	if err != nil || !ok {
		return false, err
	}
	return root.Role == "" && root.Scope == "repo" && root.Repo == subject.Repo, nil
}

func authorizeQuery(subject Resource, req Request) error {
	switch req.Verb {
	case core.QueryVersion, core.QueryCodexControl, core.QuerySnapshot:
		return nil
	case core.QuerySessions, core.QueryRepos:
		if subject.Role == "coordinator" || subject.Role == "repo" || subject.Role == "console" {
			return nil
		}
	}
	// Raw runtime paths/records and untracked-conversation fallback are never
	// exposed to restricted principals. A normalized event query will be added by
	// the mailbox transport instead.
	return ErrDenied
}

func ordinaryAction(req Request) bool {
	if req.Verb == core.ActionRename {
		return true
	}
	return req.Verb == core.ActionSetArchived && req.Fields["archived"] == "true"
}

const RuntimeGenerationField = "runtime_generation"

func coordinatorAction(req Request, self bool) bool {
	switch req.Verb {
	case core.ActionStart, core.ActionRename:
		return true
	case core.ActionSetArchived:
		if self {
			return false // root archival cascades; self-completion uses a separate path
		}
		return req.Fields["archived"] == "true" || req.Fields["archived"] == "false"
	case core.ActionSteer:
		verb := req.Fields[core.SteerVerb]
		if verb != core.SteerPrompt && verb != core.SteerInterject && verb != core.SteerStop && verb != core.SteerPermission {
			return false
		}
		if verb == core.SteerPermission && (strings.TrimSpace(req.Fields[core.SteerRequestID]) == "" || strings.TrimSpace(req.Fields[RuntimeGenerationField]) == "") {
			return false
		}
		return true
	}
	return false
}

func consoleAction(verb string) bool {
	switch verb {
	case core.ActionRefresh, core.ActionStart, core.ActionSteer, core.ActionRename,
		core.ActionMove, core.ActionArchive, core.ActionSetArchived, core.ActionDelete,
		core.ActionKill, core.ActionAddRepo, core.ActionRmRepo, core.ActionAgentSetRepos,
		core.ActionAddAgent, core.ActionNewRepoAgent, core.ActionNewWorkgroup,
		core.ActionCreateWorkspace:
		return true
	}
	return false
}

func (p Policy) FilterSnapshot(ctx context.Context, principal Principal, snap core.Snapshot) (core.Snapshot, error) {
	if principal.Kind == SubjectHost {
		return snap, nil
	}
	if principal.Kind == SubjectProvider {
		if p.Resolver == nil {
			return core.Snapshot{}, ErrDenied
		}
		out := snap
		out.Sessions = nil
		for _, session := range snap.Sessions {
			ok, err := p.Resolver.ProviderAllows(ctx, principal.SubjectID, Request{Route: RouteQuery, Verb: core.QuerySnapshot, ID: session.ID})
			if err != nil {
				return core.Snapshot{}, err
			}
			if ok {
				out.Sessions = append(out.Sessions, session)
			}
		}
		return out, nil
	}
	if principal.Kind != SubjectSession || p.Resolver == nil {
		return core.Snapshot{}, ErrDenied
	}
	subject, ok, err := p.Resolver.Lookup(ctx, principal.SubjectID)
	if err != nil || !ok {
		if err != nil {
			return core.Snapshot{}, err
		}
		return core.Snapshot{}, ErrDenied
	}
	if subject.Archived {
		return core.Snapshot{}, ErrDenied
	}
	out := snap
	out.Sessions = nil
	for _, session := range snap.Sessions {
		resource, found, err := p.Resolver.Lookup(ctx, session.ID)
		if err != nil {
			return core.Snapshot{}, err
		}
		visible, err := p.visible(ctx, subject, resource)
		if err != nil {
			return core.Snapshot{}, err
		}
		if !found || !visible {
			continue
		}
		session.Cwd = ""
		session.Pid = 0
		out.Sessions = append(out.Sessions, session)
	}
	return out, nil
}

func (p Policy) visible(ctx context.Context, subject, target Resource) (bool, error) {
	switch subject.Role {
	case "console":
		return true, nil
	case "coordinator":
		return target.ID == subject.ID || target.RootID == subject.ID, nil
	case "repo":
		return p.repoOwns(ctx, subject, target)
	case "":
		return target.ID == subject.ID, nil
	default:
		return false, nil
	}
}

func DeniedError(req Request) error {
	return fmt.Errorf("%w: %s %s", ErrDenied, req.Route, req.Verb)
}
