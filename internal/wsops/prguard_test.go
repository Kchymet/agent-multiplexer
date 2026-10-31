package wsops

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"amux/internal/core"
	"amux/internal/ghpr"
	"amux/internal/store"
)

// fakeLister serves open PRs by repo slug and records each query.
type fakeLister struct {
	mu    sync.Mutex
	prs   map[string][]ghpr.PR
	err   error
	calls []string
}

func (f *fakeLister) OpenPRs(_ context.Context, repo, branch string) ([]ghpr.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, repo+" "+branch)
	if f.err != nil {
		return nil, f.err
	}
	return ghpr.Filter(f.prs[repo], branch), nil
}

func useLister(t *testing.T, l ghpr.Lister) {
	t.Helper()
	prev := PRLister
	PRLister = l
	t.Cleanup(func() { PRLister = prev })
}

// prGuardFixture: workgroup wg with members m1 (repo api) and m2 (repo web),
// both GitHub-backed, plus a local-only repo on m1.
func prGuardFixture(t *testing.T) {
	t.Helper()
	isolateStore(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, r := range []store.Repo{
		{Name: "api", Source: "https://github.com/o/api.git", GitDir: "/unused/api"},
		{Name: "web", Source: "git@github.com:o/web.git", GitDir: "/unused/web"},
		{Name: "local", Source: "/srv/git/local.git", GitDir: "/unused/local"},
	} {
		if err := db.PutRepo(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []store.Session{
		{ID: "wg", Scope: store.ScopeWork, Dir: store.CoordinatorDir("wg")},
		{ID: "m1", RootID: "wg", Repo: "api,local", Branch: core.BranchFor("wg", "m1"), Dir: "/x/m1"},
		{ID: "m2", RootID: "wg", Repo: "web", Branch: core.BranchFor("wg", "m2"), Dir: "/x/m2"},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
}

func archive(id string, extra ...string) core.Action {
	f := map[string]string{"archived": "true"}
	for i := 0; i+1 < len(extra); i += 2 {
		f[extra[i]] = extra[i+1]
	}
	return core.Action{Action: core.ActionSetArchived, ID: id, Fields: f}
}

func TestOpenPRGuardRefusesArchiveAndKeepsEngine(t *testing.T) {
	prGuardFixture(t)
	useLister(t, &fakeLister{prs: map[string][]ghpr.PR{
		"o/api": {{Number: 7, URL: "https://github.com/o/api/pull/7", HeadRefName: "amux/wg-m1-b"}},
	}})
	stopped := false
	_, err := Dispatch(context.Background(), archive("m1"), func(string) { stopped = true })
	var open *OpenPRError
	if !errors.As(err, &open) || len(open.PRs) != 1 || open.PRs[0].SessionID != "m1" {
		t.Fatalf("archive with open PR: %v", err)
	}
	for _, want := range []string{"https://github.com/o/api/pull/7", "--force", "force=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if stopped {
		t.Fatal("refused archive stopped the engine")
	}
	if getSession(t, "m1").Archived {
		t.Fatal("refused archive committed")
	}
	// The sibling without a PR archives normally.
	if _, err := Dispatch(context.Background(), archive("m2"), nil); err != nil {
		t.Fatalf("archive without PR: %v", err)
	}
}

func TestOpenPRGuardForceBypasses(t *testing.T) {
	prGuardFixture(t)
	l := &fakeLister{prs: map[string][]ghpr.PR{
		"o/api": {{Number: 7, URL: "u7", HeadRefName: "amux/wg-m1"}},
	}}
	useLister(t, l)
	if _, err := Dispatch(context.Background(), archive("m1", FieldForce, "true"), nil); err != nil {
		t.Fatalf("forced archive: %v", err)
	}
	if !getSession(t, "m1").Archived {
		t.Fatal("forced archive did not commit")
	}
	if len(l.calls) != 0 {
		t.Fatalf("forced archive still queried gh: %v", l.calls)
	}
}

func TestOpenPRGuardFailsOpen(t *testing.T) {
	prGuardFixture(t)
	useLister(t, &fakeLister{err: errors.New("gh: not logged in")})
	if _, err := Dispatch(context.Background(), archive("wg"), nil); err != nil {
		t.Fatalf("gh error blocked archive: %v", err)
	}
	if !getSession(t, "wg").Archived {
		t.Fatal("archive not committed")
	}
}

func TestOpenPRGuardWorkgroupCascade(t *testing.T) {
	prGuardFixture(t)
	l := &fakeLister{prs: map[string][]ghpr.PR{
		"o/web": {{Number: 3, URL: "https://github.com/o/web/pull/3", HeadRefName: "amux/wg-m2"}},
		"o/api": {{Number: 9, URL: "https://github.com/o/api/pull/9", HeadRefName: "amux/other-m9"}},
	}}
	useLister(t, l)
	_, err := Dispatch(context.Background(), archive("wg"), nil)
	var open *OpenPRError
	if !errors.As(err, &open) || len(open.PRs) != 1 || open.PRs[0].SessionID != "m2" || open.PRs[0].Number != 3 {
		t.Fatalf("workgroup archive: %v", err)
	}
	if getSession(t, "wg").Archived {
		t.Fatal("refused workgroup archive committed")
	}
	// Both members' GitHub repos were checked; the local-only repo never was.
	got := strings.Join(l.calls, ";")
	for _, want := range []string{"o/api amux/wg-m1", "o/web amux/wg-m2"} {
		if !strings.Contains(got, want) {
			t.Errorf("calls %q lack %q", got, want)
		}
	}
	if len(l.calls) != 2 {
		t.Errorf("calls = %v", l.calls)
	}
}

func TestOpenPRGuardNoPRsAllowsAndUnguardedVerbs(t *testing.T) {
	prGuardFixture(t)
	l := &fakeLister{}
	useLister(t, l)
	if _, err := Dispatch(context.Background(), archive("m1"), nil); err != nil {
		t.Fatalf("archive with no PRs: %v", err)
	}
	// Restore and rename never consult GitHub.
	l.calls = nil
	l.prs = map[string][]ghpr.PR{"o/api": {{Number: 1, URL: "u", HeadRefName: "amux/wg-m1"}}}
	if _, err := Dispatch(context.Background(), core.Action{Action: core.ActionSetArchived, ID: "m1", Fields: map[string]string{"archived": "false"}}, nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := Dispatch(context.Background(), core.Action{Action: core.ActionRename, ID: "m1", Fields: map[string]string{"name": "n"}}, nil); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(l.calls) != 0 {
		t.Fatalf("unguarded verbs queried gh: %v", l.calls)
	}
}

func TestOpenPRGuardRefusesDelete(t *testing.T) {
	prGuardFixture(t)
	useLister(t, &fakeLister{prs: map[string][]ghpr.PR{
		"o/api": {{Number: 7, URL: "https://github.com/o/api/pull/7", HeadRefName: "amux/wg-m1"}},
	}})
	for _, verb := range []string{core.ActionDelete, core.ActionKill} {
		stopped := false
		_, err := Dispatch(context.Background(), core.Action{Action: verb, ID: "wg"}, func(string) { stopped = true })
		var open *OpenPRError
		if !errors.As(err, &open) || open.Verb != "delete" || !strings.Contains(err.Error(), "amux workgroup rm wg --force") {
			t.Fatalf("%s with open PR: %v", verb, err)
		}
		if stopped {
			t.Fatalf("refused %s stopped the engine", verb)
		}
	}
	if err := GuardOpenPRs(context.Background(), core.Action{Action: core.ActionDelete, ID: "wg", Fields: map[string]string{FieldForce: "true"}}); err != nil {
		t.Fatalf("forced delete guarded: %v", err)
	}
}
