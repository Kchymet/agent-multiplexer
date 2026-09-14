package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

func TestDiscoveryPagesCompleteLargeListing(t *testing.T) {
	var rows []core.AgentSessionRow
	for i := 0; i < 1000; i++ {
		rows = append(rows, core.AgentSessionRow{Harness: "claude", ID: fmt.Sprint(i), Path: fmt.Sprintf("/host/%04d.jsonl", i), Cwd: strings.Repeat("x", 500)})
	}
	cursor := ""
	seen := map[string]bool{}
	pages := 0
	for {
		body, err := discoveryPage(rows, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > sessionrpc.MaxResponseBody {
			t.Fatalf("page size %d", len(body))
		}
		var page core.AgentSessionsPage
		if err := json.Unmarshal(body, &page); err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Sessions {
			if seen[row.ID] {
				t.Fatal("duplicate row")
			}
			seen[row.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		if cursor == page.NextCursor || pages > 100 {
			t.Fatal("no progress")
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(rows) || pages < 2 {
		t.Fatalf("rows=%d pages=%d", len(seen), pages)
	}
}

func TestDiscoveryIsReadOnlyCrossSessionException(t *testing.T) {
	own := store.Session{ID: "own", RootID: "root", Agent: "claude", Dir: t.TempDir()}
	peer := store.Session{ID: "peer", RootID: "elsewhere", Agent: "claude", Dir: t.TempDir()}
	_, runtime, principals := sessionRuntimeFixture(t, own, peer)
	path := claudecfg.At(claudecfg.AgentHome(peer.Dir)).TranscriptPath("/peer-work", "peer-runtime")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"cwd":"/peer-work"}`), 0600); err != nil {
		t.Fatal(err)
	}
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QueryAgentSessions}
	result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{Principal: principals[own.ID], Call: call})
	if err != nil || result.Status != sessionrpc.StatusOK || !strings.Contains(string(result.Body), "peer-runtime") {
		t.Fatalf("discovery: %+v %v", result, err)
	}
	for _, bad := range []access.Request{
		{Route: access.RouteAction, Verb: core.ActionRename, ID: peer.ID, Fields: map[string]string{"name": "bad"}},
		{Route: access.RouteAction, Verb: core.ActionSetArchived, ID: peer.ID, Fields: map[string]string{"archived": "true"}},
		{Route: access.RouteAction, Verb: core.ActionDelete, ID: own.ID},
		{Route: access.RouteQuery, Verb: core.QueryRuntimeRecord, ID: peer.ID},
	} {
		if err := runtime.policy.Authorize(context.Background(), principals[own.ID], bad); err == nil {
			t.Fatalf("allowed %+v", bad)
		}
	}
	for _, bad := range []access.Request{
		{Route: access.RouteQuery, Verb: core.QueryAgentSessions, ID: peer.ID},
		{Route: access.RouteQuery, Verb: core.QueryAgentSessions, Fields: map[string]string{"path": "/host"}},
		{Route: access.RouteQuery, Verb: core.QueryAgentSessions, Fields: map[string]string{"cursor": "invalid"}},
		{Route: access.RouteAction, Verb: core.QueryAgentSessions},
	} {
		if _, err := canonicalSessionOperation(bad); err == nil {
			t.Fatalf("allowed %+v", bad)
		}
	}
	// An arbitrary lexical cursor can only remove rows; it never becomes a read path.
	cursor := base64.RawURLEncoding.EncodeToString([]byte("zz\x00/etc/shadow"))
	body, err := discoveryPage(nil, cursor)
	if err != nil || strings.Contains(string(body), "shadow") {
		t.Fatalf("cursor opened or disclosed: %s %v", body, err)
	}
}
