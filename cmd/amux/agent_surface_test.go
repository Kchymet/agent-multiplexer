package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

func TestAgentNameAliasesIgnoreEnvironmentIdentity(t *testing.T) {
	for _, alias := range []string{"name", "label"} {
		for _, envID := range []string{"", "foreign"} {
			t.Run(alias+"/"+envID, func(t *testing.T) {
				t.Setenv("AMUX_WORKGROUP", envID)
				t.Setenv("AMUX_WORKSPACE", "foreign")
				old := loadAgentSessionContext
				loadAgentSessionContext = func() (access.SessionContext, error) { return access.SessionContext{SubjectID: "own"}, nil }
				t.Cleanup(func() { loadAgentSessionContext = old })
				rpc := &fakeRestrictedRPC{action: sessionrpc.Result{Body: []byte(`{"ok":true}`)}}
				installRestrictedRPC(t, rpc)
				if err := cmdAgent([]string{alias, "a", "name"}); err != nil {
					t.Fatal(err)
				}
				if len(rpc.actions) != 1 || rpc.actions[0].ID != "own" || rpc.actions[0].Fields["name"] != "a name" {
					t.Fatalf("actions = %+v", rpc.actions)
				}
			})
		}
	}
}

func TestAgentDiscoveryUsesMailboxAndPropagatesFailure(t *testing.T) {
	body, _ := json.Marshal(core.AgentSessionsPage{Sessions: []core.AgentSessionRow{{Harness: "claude", ID: "foreign", Path: "/host/foreign.jsonl"}}})
	rpc := &fakeRestrictedRPC{query: sessionrpc.Result{Body: body}}
	installRestrictedRPC(t, rpc)
	if err := cmdAgent([]string{"sessions", "--json"}); err != nil {
		t.Fatal(err)
	}
	if rpc.lastQuery.Verb != core.QueryAgentSessions || rpc.lastQuery.ID != "" {
		t.Fatalf("query = %+v", rpc.lastQuery)
	}
	rpc.queryErr = errors.New("daemon unavailable")
	if err := cmdAgent([]string{"sessions"}); !errors.Is(err, rpc.queryErr) {
		t.Fatalf("failure lost: %v", err)
	}
}

// The CLI must print one complete legacy array, not a transport page or a
// partial listing when a later page fails.
type discoveryPageRPC struct {
	fakeRestrictedRPC
	pages []sessionrpc.Result
}

func (f *discoveryPageRPC) Query(ctx context.Context, query sessionrpc.Query) (sessionrpc.Result, error) {
	f.query = f.pages[f.queryCalls]
	return f.fakeRestrictedRPC.Query(ctx, query)
}
func TestAgentDiscoveryCollectsPagesBeforePrinting(t *testing.T) {
	first, _ := json.Marshal(core.AgentSessionsPage{Sessions: []core.AgentSessionRow{{ID: "older", Modified: time.Unix(1, 0)}}, NextCursor: "next"})
	last, _ := json.Marshal(core.AgentSessionsPage{Sessions: []core.AgentSessionRow{{ID: "newer", Modified: time.Unix(2, 0)}}})
	rpc := &discoveryPageRPC{pages: []sessionrpc.Result{{Body: first}, {Body: last}}}
	installRestrictedRPC(t, rpc)
	out, err := captureOutput(t, func() error { return cmdAgentSessions([]string{"--json"}) })
	var rows []core.AgentSessionRow
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 || rows[0].ID != "newer" || rpc.queries[1].Fields["cursor"] != "next" {
		t.Fatalf("out=%s err=%v queries=%+v", out, err, rpc.queries)
	}
	rpc.queryCalls = 0
	rpc.pages[1].Body = nil
	out, err = captureOutput(t, func() error { return cmdAgentSessions([]string{"--json"}) })
	if err == nil || out != "" {
		t.Fatalf("partial result published: %q %v", out, err)
	}
}
