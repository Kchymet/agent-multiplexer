package main

import (
	"context"
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

func TestAgentEventsForwardsCanonicalCursor(t *testing.T) {
	body, _ := json.Marshal(core.RuntimeEventPage{Target: "self"})
	rpc := &fakeRestrictedRPC{query: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	token := strings.Repeat("A", 43)
	_ = captureAgentEventsStdout(t, func() error {
		return cmdAgentEvents([]string{"self", "--cursor", token, "--json"})
	})
	if got := rpc.lastQuery.Fields[core.RuntimeEventsCursorField]; got != token {
		t.Fatalf("cursor field = %q, want %q", got, token)
	}
}

func TestAgentEventsPreservesFlagPresenceAndRejectsEmptyOrDuplicatePositions(t *testing.T) {
	for _, args := range [][]string{
		{"self", "--cursor="},
		{"self", "--cursor", "   "},
		{"self", "--cursor", strings.Repeat("a", 42) + "b"},
		{"self", "--cursor=", "--after", "1"},
		{"self", "--cursor", "one", "--cursor", "two"},
		{"self", "--after", "1", "--after=2"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			rpc := &fakeRestrictedRPC{}
			installRestrictedRPC(t, rpc)
			if err := cmdAgentEvents(args); err == nil {
				t.Fatalf("args %q unexpectedly accepted", args)
			}
			if rpc.queryCalls != 0 {
				t.Fatalf("args %q made %d query calls", args, rpc.queryCalls)
			}
		})
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

// scriptedEventsRPC answers successive runtime-event queries from a script.
type scriptedEventsRPC struct {
	fakeRestrictedRPC
	replies []func() (sessionrpc.Result, error)
}

func (s *scriptedEventsRPC) Query(_ context.Context, query sessionrpc.Query) (sessionrpc.Result, error) {
	s.queries = append(s.queries, query)
	reply := s.replies[0]
	s.replies = s.replies[1:]
	return reply()
}

// TestAgentEventsResumesExpiredCursorAfterSequence pins that a cursor this
// client was issued, once expired in the daemon, resumes the read with
// after_sequence at the page's scanned sequence instead of failing.
func TestAgentEventsResumesExpiredCursorAfterSequence(t *testing.T) {
	cursor := strings.Repeat("c", 43)
	page := func(scanned int64, next string) func() (sessionrpc.Result, error) {
		return func() (sessionrpc.Result, error) {
			body, _ := json.Marshal(core.RuntimeEventPage{Target: "member-1", ScannedSequence: scanned, NextCursor: next})
			return sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}, nil
		}
	}
	expired := func() (sessionrpc.Result, error) {
		return sessionrpc.Result{}, &sessionrpc.RemoteError{Status: sessionrpc.StatusInvalid, Code: "runtime_events_cursor_invalid"}
	}
	rpc := &scriptedEventsRPC{replies: []func() (sessionrpc.Result, error){page(19, cursor), expired, page(25, strings.Repeat("d", 43))}}
	installRestrictedRPC(t, rpc)
	_ = captureAgentEventsStdout(t, func() error { return cmdAgentEvents([]string{"member-1", "--json"}) })
	_ = captureAgentEventsStdout(t, func() error { return cmdAgentEvents([]string{"member-1", "--cursor", cursor, "--json"}) })
	if len(rpc.queries) != 3 || rpc.queries[1].Fields[core.RuntimeEventsCursorField] != cursor ||
		rpc.queries[2].Fields[core.RuntimeEventsAfterSequenceField] != "19" || len(rpc.queries[2].Fields) != 1 {
		t.Fatalf("queries = %+v", rpc.queries)
	}

	// A cursor never issued to this client (or for another target) still fails.
	rpc.replies = []func() (sessionrpc.Result, error){expired}
	if err := cmdAgentEvents([]string{"member-2", "--cursor", cursor}); err == nil || !strings.Contains(err.Error(), "--after") {
		t.Fatalf("unknown expired cursor = %v", err)
	}
}
