package daemon

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// The sessions read model carries each workgroup's repository ceiling, so a
// listing can say which repos a workgroup is limited to; a repo home carries
// none, and a legacy coordinator with no ceiling is reported as ungranted
// rather than as granting everything.
func TestQuerySessionsReportsWorkgroupGrants(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api", "web"} {
		if err := db.PutRepo(store.Repo{Name: name, Source: "octo/" + name, GitDir: "/x/" + name + ".git"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []store.Session{
		{ID: "one", Name: "api only", Scope: store.ScopeWork, Agent: "claude", Dir: store.CoordinatorDir("one"), Created: 1},
		{ID: "a1", RootID: "one", Agent: "claude", Repo: "api", Created: 2},
		{ID: "legacy", Scope: store.ScopeWork, Agent: "claude", Dir: store.CoordinatorDir("legacy"), Created: 3},
		{ID: "api", Scope: store.ScopeRepo, Repo: "api", Agent: "claude", Dir: store.RootDir("api"), Created: 4},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetCoordinatorRepoGrants("one", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	d := testDaemon(t)
	rows, err := d.readModelContext(context.Background(), core.Action{Action: core.ActionQuery, Query: core.QuerySessions})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]core.WorkgroupRow{}
	for _, r := range rows.([]core.WorkgroupRow) {
		byID[r.ID] = r
	}
	if one := byID["one"]; !one.Granted || !reflect.DeepEqual(one.Grants, []string{"api"}) || len(one.Agents) != 1 {
		t.Fatalf("single-repo workgroup row = %+v", one)
	}
	if legacy := byID["legacy"]; legacy.Granted || len(legacy.Grants) != 0 {
		t.Fatalf("ungranted legacy coordinator row = %+v", legacy)
	}
	if home := byID["api"]; home.Role != store.RoleRepo || home.Granted || len(home.Grants) != 0 {
		t.Fatalf("repo home row = %+v", home)
	}
}
