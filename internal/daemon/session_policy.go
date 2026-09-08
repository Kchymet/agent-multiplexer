package daemon

import (
	"context"
	"fmt"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
)

// policyStore is the read-only store surface needed to resolve session
// authority. Keeping it small lets policy/projection tests use an in-memory
// catalog and, more importantly, prevents an authorization lookup from running
// any of the legacy resolve helpers that create or migrate session state.
type policyStore interface {
	GetSession(string) (store.Session, bool, error)
	Repo(string) (store.Repo, bool, error)
	Repos() ([]store.Repo, error)
	Roots() ([]store.Session, error)
	Children(string) ([]store.Session, error)
	CoordinatorRepoGrants(string) ([]string, bool, error)
}

type policyStoreHandle struct {
	policyStore
	close func() error
}

type policyStoreOpen func() (policyStoreHandle, error)

func openPolicyStore() (policyStoreHandle, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return policyStoreHandle{}, err
	}
	return policyStoreHandle{policyStore: db, close: db.Close}, nil
}

// daemonAccessResolver derives every role and grant from current daemon-owned
// records. Provider authorization is deliberately an injected seam owned by the
// provider integration; a missing callback denies rather than treating provider
// reachability as authority.
type daemonAccessResolver struct {
	open           policyStoreOpen
	providerAllows func(context.Context, string, access.Request) (bool, error)
}

func newDaemonAccessResolver() *daemonAccessResolver {
	return &daemonAccessResolver{open: openPolicyStore}
}

func (r *daemonAccessResolver) withStore(fn func(policyStore) error) error {
	if r == nil || r.open == nil {
		return fmt.Errorf("session policy store unavailable")
	}
	h, err := r.open()
	if err != nil {
		return err
	}
	if h.close != nil {
		defer h.close()
	}
	return fn(h.policyStore)
}

func (r *daemonAccessResolver) Lookup(ctx context.Context, id string) (access.Resource, bool, error) {
	if err := ctx.Err(); err != nil {
		return access.Resource{}, false, err
	}
	var (
		resource access.Resource
		found    bool
	)
	err := r.withStore(func(db policyStore) error {
		s, ok, err := lookupPolicySession(db, id)
		if err != nil || !ok {
			return err
		}
		resource = policyResource(s)
		found = true
		return nil
	})
	return resource, found, err
}

func (r *daemonAccessResolver) RepoGranted(ctx context.Context, subjectID, repo string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	granted := false
	err := r.withStore(func(db policyStore) error {
		subject, ok, err := lookupPolicySession(db, subjectID)
		if err != nil || !ok || subject.Archived {
			return err
		}
		if _, ok, err := db.Repo(repo); err != nil || !ok {
			return err
		}
		switch subject.Role() {
		case store.RoleCoordinator:
			grants, initialized, grantErr := db.CoordinatorRepoGrants(subject.ID)
			if grantErr != nil {
				return grantErr
			}
			granted = initialized && containsString(grants, repo)
		case store.RoleRepo:
			granted = subject.Repo == repo
		case store.RoleConsole:
			granted = true
		}
		return nil
	})
	return granted, err
}

func (r *daemonAccessResolver) ProviderAllows(ctx context.Context, subject string, req access.Request) (bool, error) {
	if r == nil || r.providerAllows == nil {
		return false, nil
	}
	return r.providerAllows(ctx, subject, req)
}

func lookupPolicySession(db policyStore, id string) (store.Session, bool, error) {
	if id == "" {
		return store.Session{}, false, nil
	}
	if id == console.ID {
		// console.Session is a pure projection. Do not call console.Ensure or
		// wsops.ResolveSession from authorization: a read must not create/migrate.
		return console.Session(), true, nil
	}
	return db.GetSession(id)
}

func policyResource(s store.Session) access.Resource {
	return access.Resource{
		ID: s.ID, RootID: s.RootID, Role: s.Role(), Scope: s.Scope,
		Repo: s.Repo, Archived: s.Archived,
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// scopedSessionRows is the normalized QuerySessions projection. It omits every
// host path and branch value, and evaluates membership from one current catalog
// read immediately before the result is constructed.
func (r *daemonAccessResolver) scopedSessionRows(ctx context.Context, principal access.Principal) ([]core.WorkgroupRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []core.WorkgroupRow
	err := r.withStore(func(db policyStore) error {
		subject, ok, err := lookupPolicySession(db, principal.SubjectID)
		if err != nil {
			return err
		}
		if principal.Kind != access.SubjectSession || !ok || subject.Archived {
			return access.ErrDenied
		}
		roots, err := db.Roots()
		if err != nil {
			return err
		}
		for _, root := range roots {
			children, err := db.Children(root.ID)
			if err != nil {
				return err
			}
			rootVisible, err := policyVisible(db, subject, root)
			if err != nil {
				return err
			}
			row := normalizedWorkgroupRow(root)
			for _, child := range children {
				visible, err := policyVisible(db, subject, child)
				if err != nil {
					return err
				}
				if visible {
					row.Agents = append(row.Agents, normalizedAgentRow(child))
				}
			}
			if rootVisible || len(row.Agents) != 0 {
				rows = append(rows, row)
			}
		}
		return nil
	})
	return rows, err
}

// scopedRepoRows exposes names only. Source may be a local host path or embed
// credentials, and GitDir is intentionally not a wire field at all.
func (r *daemonAccessResolver) scopedRepoRows(ctx context.Context, principal access.Principal) ([]core.RepoRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows []core.RepoRow
	err := r.withStore(func(db policyStore) error {
		subject, ok, err := lookupPolicySession(db, principal.SubjectID)
		if err != nil {
			return err
		}
		if principal.Kind != access.SubjectSession || !ok || subject.Archived {
			return access.ErrDenied
		}
		repos, err := db.Repos()
		if err != nil {
			return err
		}
		var coordinatorGrants []string
		coordinatorGrantsInitialized := false
		if subject.Role() == store.RoleCoordinator {
			coordinatorGrants, coordinatorGrantsInitialized, err = db.CoordinatorRepoGrants(subject.ID)
			if err != nil {
				return err
			}
		}
		for _, repo := range repos {
			visible := false
			switch subject.Role() {
			case store.RoleConsole:
				visible = true
			case store.RoleCoordinator:
				visible = coordinatorGrantsInitialized && containsString(coordinatorGrants, repo.Name)
			case store.RoleRepo:
				visible = subject.Repo == repo.Name
			}
			if visible {
				rows = append(rows, core.RepoRow{Name: repo.Name})
			}
		}
		return nil
	})
	return rows, err
}

func policyVisible(db policyStore, subject, target store.Session) (bool, error) {
	switch subject.Role() {
	case store.RoleConsole:
		return true, nil
	case store.RoleCoordinator:
		return target.ID == subject.ID || target.RootID == subject.ID, nil
	case store.RoleRepo:
		if target.ID == subject.ID {
			return true, nil
		}
		if target.RootID == "" {
			return target.Role() == store.RoleAgent && target.Scope == store.ScopeRepo && target.Repo == subject.Repo, nil
		}
		root, ok, err := db.GetSession(target.RootID)
		if err != nil || !ok {
			return false, err
		}
		return root.Role() == store.RoleAgent && root.Scope == store.ScopeRepo && root.Repo == subject.Repo, nil
	case store.RoleAgent:
		return target.ID == subject.ID, nil
	default:
		return false, nil
	}
}

func normalizedWorkgroupRow(root store.Session) core.WorkgroupRow {
	scope := root.Scope
	if scope == "" {
		scope = store.ScopeWork
	}
	return core.WorkgroupRow{
		ID: root.ID, Scope: scope, Display: root.Display(),
		Role: root.Role(), Agent: root.Agent,
	}
}

func normalizedAgentRow(s store.Session) core.AgentRow {
	return core.AgentRow{
		ID: s.ID, Agent: s.Agent, Mode: s.Mode, Repos: s.Repo,
		Archived: s.Archived,
	}
}

func normalizeRestrictedSnapshot(snap core.Snapshot) core.Snapshot {
	for i := range snap.Sessions {
		s := &snap.Sessions[i]
		s.Cwd = ""
		s.Pid = 0
		s.CanAttach = false
		s.CanKill = false
		s.CanResume = false
		// Host snapshots commonly decorate Status with a branch name. Restricted
		// consumers receive only the normalized activity state.
		s.Status = s.State
	}
	return snap
}
