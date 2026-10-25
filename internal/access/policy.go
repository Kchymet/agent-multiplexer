package access

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
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
	// Default deny: an operation without an explicit classification is never
	// reachable by a restricted principal, whatever its role.
	rule, classified := RuleFor(req.Route, req.Verb)
	if !classified {
		return ErrDenied
	}
	// The subject is the authenticated grant's store subject. Request IDs only
	// ever name the target; they never select the caller.
	subject, ok, err := p.Resolver.Lookup(ctx, principal.SubjectID)
	if err != nil {
		return err
	}
	if !ok || subject.Archived || !knownRole(subject.Role) {
		return ErrDenied
	}

	switch rule.Scope {
	case ScopeSession, ScopeFiltered:
		// ScopeFiltered handlers project the result to InWorkgroupScope(subject).
		return nil
	case ScopeHost:
		if subject.Role == "console" && rule.Console {
			return nil
		}
		if subject.Role == "repo" && req.Verb == core.ActionNewWorkgroup && req.ID == "" && repoScopedWorkgroup(subject, req) {
			return nil
		}
		return ErrDenied
	case ScopeWorkgroup:
	default:
		return ErrDenied
	}

	if strings.TrimSpace(req.ID) == "" {
		return ErrDenied
	}
	// A self request reuses the subject row: one read decides both sides.
	target, targetOK := subject, req.ID == subject.ID
	if !targetOK {
		if target, targetOK, err = p.Resolver.Lookup(ctx, req.ID); err != nil {
			return err
		}
	}
	if !targetOK {
		return ErrDenied
	}
	if subject.Role == "console" && rule.Console {
		return nil
	}
	self := target.ID == subject.ID
	if !InWorkgroupScope(subject, target) {
		return ErrDenied
	}
	// Reads may page an archived member while it still belongs to the scope;
	// effects never target an archived session (restore is host-only).
	if req.Route == RouteAction && target.Archived {
		return ErrDenied
	}
	if rule.Allow != nil && !rule.Allow(req, self, target) {
		return ErrDenied
	}
	if req.Verb == core.ActionAddAgent && subject.Role != "console" {
		return p.authorizeAddAgent(ctx, subject, req)
	}
	return nil
}

// InWorkgroupScope reports whether target is within subject's workgroup scope:
// itself, or for a coordinator, a current direct member of its workgroup. The
// console is the operator's delegated global view. Ordinary agents and repo
// homes are self-only: sharing a repository does not confer access to a
// workgroup, whose coordinator owns its member sessions. Membership is the
// target's current daemon-owned RootID, so a member moved out of the workgroup
// leaves the scope immediately.
func InWorkgroupScope(subject, target Resource) bool {
	switch subject.Role {
	case "console":
		return true
	case "coordinator":
		return target.ID == subject.ID || target.RootID == subject.ID
	case "repo", "":
		return target.ID == subject.ID
	default:
		return false
	}
}

func knownRole(role string) bool {
	switch role {
	case "", "console", "coordinator", "repo":
		return true
	}
	return false
}

func repoScopedWorkgroup(subject Resource, req Request) bool {
	repos := splitList(req.Fields["repos"])
	return len(repos) == 1 && repos[0] == subject.Repo
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

const RuntimeGenerationField = harnessproto.FieldRuntimeGeneration

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
		if !found || !InWorkgroupScope(subject, resource) {
			continue
		}
		session.Cwd = ""
		session.Pid = 0
		out.Sessions = append(out.Sessions, session)
	}
	return out, nil
}

func DeniedError(req Request) error {
	return fmt.Errorf("%w: %s %s", ErrDenied, req.Route, req.Verb)
}
