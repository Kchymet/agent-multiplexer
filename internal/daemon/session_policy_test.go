package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/store"
)

type fakePolicyStore struct {
	sessions          map[string]store.Session
	repos             map[string]store.Repo
	coordinatorRepos  map[string][]string
	grantsInitialized map[string]bool
}

func (f *fakePolicyStore) CoordinatorRepoGrants(id string) ([]string, bool, error) {
	return append([]string(nil), f.coordinatorRepos[id]...), f.grantsInitialized[id], nil
}

func (f *fakePolicyStore) GetSession(id string) (store.Session, bool, error) {
	s, ok := f.sessions[id]
	return s, ok, nil
}

func (f *fakePolicyStore) Repo(name string) (store.Repo, bool, error) {
	r, ok := f.repos[name]
	return r, ok, nil
}

func (f *fakePolicyStore) Repos() ([]store.Repo, error) {
	var out []store.Repo
	for _, repo := range f.repos {
		out = append(out, repo)
	}
	return out, nil
}

func (f *fakePolicyStore) Roots() ([]store.Session, error) {
	var out []store.Session
	for _, s := range f.sessions {
		if s.RootID == "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakePolicyStore) Children(rootID string) ([]store.Session, error) {
	var out []store.Session
	for _, s := range f.sessions {
		if s.RootID == rootID {
			out = append(out, s)
		}
	}
	return out, nil
}

func fakeResolver(db *fakePolicyStore) *daemonAccessResolver {
	return &daemonAccessResolver{open: func() (policyStoreHandle, error) {
		return policyStoreHandle{policyStore: db, close: func() error { return nil }}, nil
	}}
}

func policyFixture() *fakePolicyStore {
	return &fakePolicyStore{
		sessions: map[string]store.Session{
			"wg1":       {ID: "wg1", Scope: store.ScopeWork, Repo: "api,web", Agent: "codex", Dir: "/secret/wg1"},
			"a1":        {ID: "a1", RootID: "wg1", Repo: "api", Agent: "codex", Mode: "task", Branch: "amux/a1", Dir: "/secret/a1"},
			"old":       {ID: "old", RootID: "wg1", Repo: "web", Archived: true, Branch: "amux/old", Dir: "/secret/old"},
			"wg2":       {ID: "wg2", Scope: store.ScopeWork, Repo: "api", Agent: "claude", Dir: "/secret/wg2"},
			"a2":        {ID: "a2", RootID: "wg2", Repo: "api", Agent: "claude", Dir: "/secret/a2"},
			"api":       {ID: "api", Scope: store.ScopeRepo, Repo: "api", Agent: "codex", Dir: "/secret/api-home"},
			"repo-root": {ID: "repo-root", Scope: store.ScopeRepo, Repo: "api", Dir: "/secret/repo-root"},
			"oneoff":    {ID: "oneoff", RootID: "repo-root", Repo: "api", Agent: "codex", Mode: "task", Branch: "amux/oneoff", Dir: "/secret/oneoff"},
		},
		repos: map[string]store.Repo{
			"api":    {Name: "api", Source: "https://token@example/api.git", GitDir: "/secret/repos/api.git"},
			"web":    {Name: "web", Source: "/host/repos/web", GitDir: "/secret/repos/web.git"},
			"hidden": {Name: "hidden", Source: "/host/repos/hidden", GitDir: "/secret/repos/hidden.git"},
		},
		coordinatorRepos:  map[string][]string{"wg1": {"api", "web"}, "wg2": {"api"}},
		grantsInitialized: map[string]bool{"wg1": true, "wg2": true},
	}
}

func TestDaemonAccessResolverUsesCurrentStoredRoleAndGrant(t *testing.T) {
	db := policyFixture()
	r := fakeResolver(db)

	got, ok, err := r.Lookup(context.Background(), "a1")
	if err != nil || !ok {
		t.Fatalf("Lookup(a1) = %+v, %v, %v", got, ok, err)
	}
	if got.ID != "a1" || got.RootID != "wg1" || got.Role != store.RoleAgent || got.Repo != "api" {
		t.Fatalf("Lookup(a1) = %+v", got)
	}
	if yes, err := r.RepoGranted(context.Background(), "wg1", "api"); err != nil || !yes {
		t.Fatalf("RepoGranted(api) = %v, %v", yes, err)
	}
	if yes, err := r.RepoGranted(context.Background(), "wg1", "hidden"); err != nil || yes {
		t.Fatalf("RepoGranted(hidden) = %v, %v", yes, err)
	}

	// Re-read current state on every decision: archive and grant removal take
	// effect without retaining the old Resource or repository ceiling.
	s := db.sessions["wg1"]
	s.Archived = true
	db.sessions["wg1"] = s
	db.coordinatorRepos["wg1"] = nil
	if yes, err := r.RepoGranted(context.Background(), "wg1", "api"); err != nil || yes {
		t.Fatalf("RepoGranted after archive = %v, %v", yes, err)
	}
	got, ok, err = r.Lookup(context.Background(), "wg1")
	if err != nil || !ok || !got.Archived {
		t.Fatalf("Lookup archived = %+v, %v, %v", got, ok, err)
	}
}

func TestDaemonAccessResolverNeverDerivesCoordinatorGrantFromChildren(t *testing.T) {
	isolateHome(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(store.Repo{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	for _, session := range []store.Session{
		{ID: "blank", Scope: store.ScopeWork},
		{ID: "blank-child", RootID: "blank", Repo: "api"},
		{ID: "empty", Scope: store.ScopeWork},
		{ID: "empty-child", RootID: "empty", Repo: "api"},
	} {
		if err := db.PutSession(session); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetCoordinatorRepoGrants("empty", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	resolver := newDaemonAccessResolver()
	for _, coordinator := range []string{"blank", "empty"} {
		granted, err := resolver.RepoGranted(context.Background(), coordinator, "api")
		if err != nil {
			t.Fatalf("RepoGranted(%s): %v", coordinator, err)
		}
		if granted {
			t.Fatalf("coordinator %s inherited api from child history", coordinator)
		}
		rows, err := resolver.scopedRepoRows(context.Background(), access.Principal{
			Kind: access.SubjectSession, SubjectID: coordinator,
		})
		if err != nil {
			t.Fatalf("scopedRepoRows(%s): %v", coordinator, err)
		}
		if len(rows) != 0 {
			t.Fatalf("coordinator %s saw repos from child history: %+v", coordinator, rows)
		}
	}

	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCoordinatorRepoGrants("empty", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if granted, err := resolver.RepoGranted(context.Background(), "empty", "api"); err != nil || !granted {
		t.Fatalf("explicit host grant was not observed: granted=%v err=%v", granted, err)
	}
}

func TestScopedCoordinatorRowsAreCurrentAndPathFree(t *testing.T) {
	db := policyFixture()
	r := fakeResolver(db)
	who := access.Principal{Kind: access.SubjectSession, SubjectID: "wg1"}

	rows, err := r.scopedSessionRows(context.Background(), who)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "wg1" {
		t.Fatalf("rows = %+v, want only wg1", rows)
	}
	if rows[0].Dir != "" {
		t.Fatalf("root leaked dir %q", rows[0].Dir)
	}
	wantIDs := map[string]bool{"a1": true, "old": true}
	for _, child := range rows[0].Agents {
		if !wantIDs[child.ID] {
			t.Fatalf("foreign child in rows: %+v", child)
		}
		delete(wantIDs, child.ID)
		if child.Dir != "" || child.Branch != "" {
			t.Fatalf("child leaked host data: %+v", child)
		}
	}
	if len(wantIDs) != 0 {
		t.Fatalf("missing children %v", wantIDs)
	}

	root := db.sessions["wg1"]
	root.Archived = true
	db.sessions["wg1"] = root
	if _, err := r.scopedSessionRows(context.Background(), who); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("archived subject error = %v, want denied", err)
	}
}

func TestScopedRepoHomeUsesAuthoritativeRepoRoot(t *testing.T) {
	r := fakeResolver(policyFixture())
	rows, err := r.scopedSessionRows(context.Background(), access.Principal{Kind: access.SubjectSession, SubjectID: "api"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, row := range rows {
		for _, child := range row.Agents {
			got[row.ID] = append(got[row.ID], child.ID)
		}
		if _, ok := got[row.ID]; !ok {
			got[row.ID] = nil
		}
	}
	if !reflect.DeepEqual(got["repo-root"], []string{"oneoff"}) {
		t.Fatalf("repo-root children = %v, want oneoff", got["repo-root"])
	}
	if _, ok := got["api"]; !ok {
		t.Fatalf("repo home missing from %v", got)
	}
	if _, ok := got["wg1"]; ok {
		t.Fatalf("same-repo workgroup leaked into repo-home scope: %v", got)
	}
	if _, ok := got["wg2"]; ok {
		t.Fatalf("unrelated same-repo workgroup leaked: %v", got)
	}
}

func TestScopedReposExposeNamesOnly(t *testing.T) {
	r := fakeResolver(policyFixture())
	rows, err := r.scopedRepoRows(context.Background(), access.Principal{Kind: access.SubjectSession, SubjectID: "wg1"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.Name] = true
		if row.Source != "" {
			t.Fatalf("repo %s leaked source %q", row.Name, row.Source)
		}
	}
	if !reflect.DeepEqual(got, map[string]bool{"api": true, "web": true}) {
		t.Fatalf("repos = %v", got)
	}
}

func TestNormalizeRestrictedSnapshotDropsHostAndStreamingAuthority(t *testing.T) {
	in := core.Snapshot{Sessions: []core.Session{{
		ID: "a1", Cwd: "/secret/a1", Pid: 42, CanAttach: true, CanKill: true, CanResume: true,
		State: core.StateReady, Status: "ready · secret-branch",
	}}}
	out := normalizeRestrictedSnapshot(in)
	got := out.Sessions[0]
	if got.Cwd != "" || got.Pid != 0 || got.CanAttach || got.CanKill || got.CanResume || got.Status != core.StateReady {
		t.Fatalf("normalized session = %+v", got)
	}
}
