package runtimeevents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestStructuredSourceIsIdentityMapped checks the AGE-181 structured record path:
// a supervisor-written NDJSON log of already-normalized events is streamed back
// verbatim (identity mapper), unlike a raw runtime transcript which is decoded.
func TestStructuredSourceIsIdentityMapped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.events.jsonl")

	events := []harnessproto.RuntimeEvent{
		{Type: harnessproto.TypeTurnStart, Direction: harnessproto.DirMeta, Payload: json.RawMessage(`{}`)},
		{Type: harnessproto.TypeText, ItemID: "m1", Direction: harnessproto.DirOut, Payload: json.RawMessage(`{"text":"hi","final":true}`)},
		{Type: harnessproto.TypePermissionRequest, ItemID: "it1", Direction: harnessproto.DirOut, Payload: json.RawMessage(`{"request_id":"ap1","tool":"command_execution"}`)},
		{Type: harnessproto.TypeTurnEnd, Direction: harnessproto.DirMeta, Payload: json.RawMessage(`{"stop_reason":"completed"}`)},
	}
	var buf []byte
	for _, e := range events {
		b, _ := json.Marshal(e)
		buf = append(buf, append(b, '\n')...)
	}
	// A blank line and a malformed line must be tolerated (skipped), like a partial
	// write to a runtime transcript.
	buf = append(buf, []byte("\n{not json}\n")...)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: path, Structured: true}
	stream := Stream(func(string) (Record, bool) { return rec, true }, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, ok := stream(ctx, "s", 0)
	if !ok {
		t.Fatal("structured stream not ok")
	}
	got := collect(t, ch)

	if len(got) != len(events) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(events), got)
	}
	if got[0].Type != harnessproto.TypeTurnStart || got[1].Type != harnessproto.TypeText ||
		got[2].Type != harnessproto.TypePermissionRequest || got[3].Type != harnessproto.TypeTurnEnd {
		t.Fatalf("event types not preserved verbatim: %+v", got)
	}
	if got[1].ItemID != "m1" || string(got[1].Payload) != `{"text":"hi","final":true}` {
		t.Fatalf("text event not identity-mapped: %+v", got[1])
	}
}

// TestStructuredOpenPermissions checks that the structured record is a permission
// source: OpenPermissions replays a permission_request from it (so the daemon can
// correlate a decision even off the durable log).
func TestStructuredOpenPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.events.jsonl")
	req, _ := json.Marshal(harnessproto.RuntimeEvent{
		Type: harnessproto.TypePermissionRequest, ItemID: "it1", Direction: harnessproto.DirOut,
		Payload: json.RawMessage(`{"request_id":"ap9","tool":"command_execution","action":"rm"}`),
	})
	if err := os.WriteFile(path, append(req, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	open := OpenPermissions(Record{Runtime: harnessproto.RuntimeCodex, Path: path, Structured: true})
	if len(open) != 1 || open[0].RequestID != "ap9" {
		t.Fatalf("OpenPermissions = %+v, want one request ap9", open)
	}
}
