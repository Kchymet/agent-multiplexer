package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Exercise the real resolver, launch access provisioning, authorization, and
// private transcript reader together. No engine or model process is started.
func TestMigratedCoordinatorResolvesOwnDirectoryAndAuthenticatedMemberHistory(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.PutRepo(store.Repo{Name: "api"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy", "unrelated"} {
		if err := db.PutSession(store.Session{ID: id, Scope: store.ScopeRepo, Repo: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	members := []store.Session{
		{ID: "active-member", RootID: "legacy", ClaudeID: "11111111-1111-4111-8111-111111111111"},
		{ID: "archived-member", RootID: "legacy", ClaudeID: "22222222-2222-4222-8222-222222222222", Archived: true, ArchivedAt: 123},
		{ID: "foreign-member", RootID: "unrelated", ClaudeID: "33333333-3333-4333-8333-333333333333"},
	}
	transcripts := map[string]string{}
	for i := range members {
		member := &members[i]
		member.Agent, member.Mode, member.Repo = "claude", store.ModeTask, "api"
		member.Dir = store.AgentDir(member.RootID, member.ID)
		member.Branch = core.BranchFor(member.RootID, member.ID)
		if err := db.PutSession(*member); err != nil {
			t.Fatal(err)
		}
		path := claudecfg.At(claudecfg.AgentHome(member.Dir)).TranscriptPath(member.Dir, member.ClaudeID)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		record := fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"preserved %s history\"}]}}\n", member.ID)
		if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
		transcripts[path] = record
	}
	if _, err := os.Stat(store.CoordinatorDir("legacy")); !os.IsNotExist(err) {
		t.Fatalf("coordinator exists before promotion: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	coordinator, found, err := wsops.ResolveSession("legacy")
	if err != nil || !found {
		t.Fatalf("resolve promoted coordinator: found=%v err=%v", found, err)
	}
	if coordinator.Role() != store.RoleCoordinator || coordinator.Dir != store.CoordinatorDir("legacy") || coordinator.Dir == store.RootDir("legacy") {
		t.Fatalf("coordinator resolved to shared container: %+v", coordinator)
	}
	info, err := os.Lstat(coordinator.Dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("coordinator directory: %v %v", info, err)
	}
	for _, member := range members {
		preserved, found, err := wsops.ResolveSession(member.ID)
		if err != nil || !found || !reflect.DeepEqual(preserved, member) {
			t.Fatalf("member changed: %+v, found=%v err=%v", preserved, found, err)
		}
	}

	d := New("", nil, 0)
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.authority.Close() })
	launch, grant, err := d.sessionAccessForLaunch(ctx, coordinator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Dir != coordinator.Dir || grant.SubjectID != coordinator.ID {
		t.Fatalf("launch access: %+v %+v", launch, grant)
	}
	credential, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, coordinator.ID))
	if err != nil {
		t.Fatal(err)
	}
	principal := credentialPrincipal(credential)
	runtime := newSessionRuntime(d)
	t.Cleanup(runtime.close)
	read := func(who access.Principal, target string) sessionrpc.DispatchResult {
		t.Helper()
		result, err := runtime.dispatch(ctx, sessionrpc.DispatchRequest{
			Principal: who, RequestID: "0123456789abcdef0123456789abcdef",
			Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: target},
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, member := range members[:2] {
		result := read(principal, member.ID)
		if result.Status != sessionrpc.StatusOK {
			t.Fatalf("authenticated history for %s: %+v", member.ID, result)
		}
		page := decodeEventPage(t, result.Body)
		foundText := false
		for _, event := range page.Events {
			if event.Event.Type != harnessproto.TypeText {
				continue
			}
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(event.Event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			foundText = foundText || payload.Text == "preserved "+member.ID+" history"
		}
		if !foundText {
			t.Fatalf("preserved transcript missing from normalized history: %+v", page)
		}
	}
	if result := read(principal, members[2].ID); result.Status != sessionrpc.StatusDenied {
		t.Fatalf("same-repo foreign history exposed: %+v", result)
	}
	invalid := principal
	invalid.Generation++
	if result := read(invalid, members[0].ID); result.Status != sessionrpc.StatusDenied {
		t.Fatalf("invalid credential read history: %+v", result)
	}
	for path, want := range transcripts {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("private transcript changed at %s: %q %v", path, got, err)
		}
	}
}
