package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
)

// A constrained coordinator always SEES every tracked repo (marked with its
// grant) but may only give members worktrees of granted ones; the refusal names
// the repo and the host command that widens grants, and host grant changes take
// effect for the very next request without a restart.
func TestCoordinatorRepoVisibilityIsNotAuthority(t *testing.T) {
	coordinator := store.Session{ID: "wg", Scope: store.ScopeWork, Agent: "claude", ClaudeID: "repo-grants"}
	_, runtime, principals := sessionRuntimeFixture(t, coordinator)
	t.Cleanup(runtime.completions.close)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"api", "web"} {
		if err := db.PutRepo(store.Repo{Name: repo}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetCoordinatorRepoGrants("wg", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := runtime.resolver.scopedRepoRows(context.Background(), principals["wg"])
	if err != nil {
		t.Fatal(err)
	}
	marks := map[string]bool{}
	for _, row := range rows {
		if row.Granted == nil {
			t.Fatalf("coordinator repo row %+v lacks a grant mark", row)
		}
		marks[row.Name] = *row.Granted
	}
	if len(marks) != 2 || !marks["api"] || marks["web"] {
		t.Fatalf("constrained coordinator repo ls = %v, want api granted and web visible but not granted", marks)
	}

	n := 0
	addAgent := func(repo string) sessionrpc.DispatchResult {
		t.Helper()
		n++
		result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{
			Principal: principals["wg"],
			RequestID: strings.Repeat("0", 31) + string(rune('a'+n)),
			Call: sessionrpc.Call{
				Kind: sessionrpc.CallOperation, Route: access.RouteAction,
				Verb: core.ActionAddAgent, ID: "wg",
				Fields: map[string]string{"agent": "claude", "repos": repo},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	expectRefused := func(repo string) {
		t.Helper()
		result := addAgent(repo)
		if result.Status != sessionrpc.StatusDenied || result.Code != codeRepoNotGranted {
			t.Fatalf("add-agent %s = %+v, want denied %s", repo, result, codeRepoNotGranted)
		}
		var body core.Result
		if err := json.Unmarshal(result.Body, &body); err != nil {
			t.Fatalf("denial body: %v", err)
		}
		if !strings.Contains(body.Error, `"`+repo+`"`) || !strings.Contains(body.Error, "amux workgroup grants wg") {
			t.Fatalf("denial message %q does not name %s and the host grants command", body.Error, repo)
		}
	}
	expectAllowed := func(repo string) {
		t.Helper()
		// Past authorization the fixture's repos have no clone to fork, so the
		// effect itself may fail; what matters here is it was not denied.
		if result := addAgent(repo); result.Status == sessionrpc.StatusDenied {
			t.Fatalf("add-agent %s = %+v, want admitted", repo, result)
		}
	}
	setGrants := func(fields map[string]string) {
		t.Helper()
		if _, err := wsops.ApplyResult(context.Background(), core.Action{Action: core.ActionCoordinatorSetRepos, ID: "wg", Fields: fields}); err != nil {
			t.Fatal(err)
		}
	}

	expectAllowed("api")
	expectRefused("web")
	setGrants(map[string]string{"repos": "api,web"}) // widen
	expectAllowed("web")
	setGrants(map[string]string{"repos": "web"}) // narrow
	expectRefused("api")
	setGrants(map[string]string{"repos": ""}) // clear
	expectRefused("web")
	setGrants(map[string]string{"all": "1"}) // reset to all tracked
	expectAllowed("api")
	expectAllowed("web")
	expectRefused("untracked")
}
