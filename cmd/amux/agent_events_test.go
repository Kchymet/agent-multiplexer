package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestAgentEventsUsesAuthenticatedPagedQuery(t *testing.T) {
	body, err := json.Marshal(core.RuntimeEventPage{
		Target: "member-1", Runtime: "codex", CursorSequence: 4, ScannedSequence: 5,
		Events: []core.SequencedRuntimeEvent{{Sequence: 5, Event: harnessproto.RuntimeEvent{
			Type: harnessproto.TypeText, Direction: harnessproto.DirOut, Payload: json.RawMessage(`{"text":"done"}`),
		}}},
		NextCursor: strings.Repeat("a", 43),
	})
	if err != nil {
		t.Fatal(err)
	}
	rpc := &fakeRestrictedRPC{query: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	out := captureAgentEventsStdout(t, func() error {
		return cmdAgentEvents([]string{"member-1", "--after", "4"})
	})
	if rpc.lastQuery.Verb != core.QueryRuntimeEvents || rpc.lastQuery.ID != "member-1" ||
		rpc.lastQuery.Fields[core.RuntimeEventsAfterSequenceField] != "4" {
		t.Fatalf("query = %+v", rpc.lastQuery)
	}
	if !strings.Contains(out, "5\ttext\tout") || !strings.Contains(out, "next cursor:") {
		t.Fatalf("output = %q", out)
	}
}

func TestAgentEventsDefaultsToFixedContextSubject(t *testing.T) {
	body, _ := json.Marshal(core.RuntimeEventPage{Target: "self", Events: []core.SequencedRuntimeEvent{}})
	rpc := &fakeRestrictedRPC{query: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	oldLoad := loadAgentSessionContext
	loadAgentSessionContext = func() (access.SessionContext, error) {
		return access.SessionContext{Protocol: access.ProtocolVersion, SubjectID: "self", MailboxDir: "/mailbox"}, nil
	}
	t.Cleanup(func() { loadAgentSessionContext = oldLoad })
	_ = captureAgentEventsStdout(t, func() error { return cmdAgentEvents([]string{"--json"}) })
	if rpc.lastQuery.ID != "self" {
		t.Fatalf("query target = %q", rpc.lastQuery.ID)
	}
}

func TestAgentEventsRejectsAmbiguousCursorLocally(t *testing.T) {
	rpc := &fakeRestrictedRPC{}
	installRestrictedRPC(t, rpc)
	if err := cmdAgentEvents([]string{"self", "--cursor", "token", "--after", "1"}); err == nil {
		t.Fatal("ambiguous cursor unexpectedly accepted")
	}
	if rpc.queryCalls != 0 {
		t.Fatalf("query calls = %d", rpc.queryCalls)
	}
}

func captureAgentEventsStdout(t *testing.T, run func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	err = run()
	_ = w.Close()
	os.Stdout = old
	if err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(b)
}
