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

// permissionPayload is the published shape of a permission_request payload; the
// tests decode into it so a renamed field fails here rather than in a consumer.
type permissionPayload struct {
	RequestID string   `json:"request_id"`
	Tool      string   `json:"tool"`
	Action    string   `json:"action"`
	Options   []string `json:"options"`
	Decision  string   `json:"decision"`
}

func decodePermission(t *testing.T, ev harnessproto.RuntimeEvent) permissionPayload {
	t.Helper()
	var p permissionPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload %s: %v", ev.Payload, err)
	}
	return p
}

// TestPermissionJournalContract maps a recorded permission journal — the record
// amux's own Claude hooks write, because Claude Code's transcript carries no
// permission prompt at all — and pins what a consumer receives: a request it can
// answer, the resolution retiring it, and a line from no known version of the
// format passed through as raw rather than dropped.
func TestPermissionJournalContract(t *testing.T) {
	events := mapEachLine("testdata/claude_permissions.jsonl", permissionMapper(harnessproto.RuntimeClaude)())
	if len(events) != 4 {
		t.Fatalf("got %d events from the 4-line journal: %+v", len(events), events)
	}

	req := decodePermission(t, events[0])
	if events[0].Type != TypePermissionRequest {
		t.Fatalf("first event = %q, want permission_request", events[0].Type)
	}
	if req.RequestID != "perm-9f2a1c4e0b7d8a55" || req.Tool != "Bash" || req.Action != "rm -rf build/" {
		t.Errorf("request payload = %+v, want the journaled id/tool/action", req)
	}
	if len(req.Options) != 2 {
		t.Errorf("options = %v, want the two amux can deliver", req.Options)
	}
	// The id is the coalescing key, so a consumer can replace the card in place.
	if events[0].ItemID != req.RequestID {
		t.Errorf("item_id = %q, want the request id %q", events[0].ItemID, req.RequestID)
	}

	res := decodePermission(t, events[1])
	if events[1].Type != TypePermissionResolved || res.RequestID != req.RequestID || res.Decision != "allow" {
		t.Errorf("second event = %q %+v, want perm-…a55 resolved allow", events[1].Type, res)
	}
	if events[2].Type != TypePermissionRequest {
		t.Errorf("third event = %q, want the second request", events[2].Type)
	}
	if events[3].Type != TypeRaw {
		t.Errorf("an unrecognized journal line = %q, want raw (never dropped)", events[3].Type)
	}

	// What the daemon reads back: exactly the one request still open. This is the
	// correlation the `permission` verb rejects a stale id against.
	rec := Record{Runtime: harnessproto.RuntimeClaude, Permissions: "testdata/claude_permissions.jsonl"}
	open := OpenPermissions(rec)
	if len(open) != 1 || open[0].RequestID != "perm-4d81b0725f3ce619" {
		t.Fatalf("open = %+v, want only the unresolved perm-4d81…", open)
	}
	if p, ok := PendingPermission(rec); !ok || p.Tool != "Write" {
		t.Errorf("PendingPermission = %+v ok=%v, want the open Write request", p, ok)
	}
	// A session that was never prompted has nothing open — not an error.
	if _, ok := PendingPermission(Record{Runtime: harnessproto.RuntimeClaude, Path: "testdata/claude_session.jsonl"}); ok {
		t.Error("a session with no journal must report no pending request")
	}
}

// TestCodexApprovalContract pins the Codex half of the same contract. Codex
// writes its approval prompts into the rollout itself, so it needs no journal —
// but it records only the prompt, never the answer, so the reader infers the
// resolution from what happens to the same call_id next.
//
// testdata/codex_approvals.jsonl is written in codex-cli 0.153's rollout shapes
// (the same envelope as codex_session.jsonl) and covers the three outcomes: a
// command that started (approved), a patch that produced output without ever
// starting (declined), and a prompt still waiting when the record ends.
func TestCodexApprovalContract(t *testing.T) {
	st := &CodexState{}
	events := mapEachLine("testdata/codex_approvals.jsonl", func(line []byte) []harnessproto.RuntimeEvent {
		return MapCodexLine(line, st)
	})

	var kinds []string
	byID := map[string][]permissionPayload{}
	for _, ev := range events {
		switch ev.Type {
		case TypePermissionRequest, TypePermissionResolved:
			kinds = append(kinds, ev.Type)
			p := decodePermission(t, ev)
			byID[p.RequestID] = append(byID[p.RequestID], p)
		}
	}
	if len(kinds) != 5 {
		t.Fatalf("permission events = %v, want request/resolve for two calls plus one open request", kinds)
	}

	// The exec prompt was approved: the command started.
	if got := byID["call_11"]; len(got) != 2 || got[1].Decision != harnessproto.DecisionAllow {
		t.Errorf("call_11 = %+v, want request then allow (exec_command_begin proves it ran)", got)
	}
	if got := byID["call_11"]; len(got) > 0 && got[0].Tool != "exec_command" {
		t.Errorf("call_11 tool = %q, want exec_command", got[0].Tool)
	}
	// The patch prompt was declined: output arrived without the patch ever starting.
	if got := byID["call_12"]; len(got) != 2 || got[1].Decision != harnessproto.DecisionDeny {
		t.Errorf("call_12 = %+v, want request then deny (output without a start)", got)
	}
	// The last prompt is still waiting, so it is the one a `permission` verb answers.
	if got := byID["call_13"]; len(got) != 1 {
		t.Errorf("call_13 = %+v, want an unresolved request", got)
	}

	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: "testdata/codex_approvals.jsonl"}
	open := OpenPermissions(rec)
	if len(open) != 1 || open[0].RequestID != "call_13" {
		t.Fatalf("open = %+v, want only call_13", open)
	}
}

// TestCodexApprovalClearedByTurnEnd: a turn cannot end with a prompt still up, so
// an approval that outlives its turn is retired as `cleared` — amux knows it is
// gone without claiming to know how it was answered.
func TestCodexApprovalClearedByTurnEnd(t *testing.T) {
	st := &CodexState{}
	lines := []string{
		`{"type":"event_msg","payload":{"type":"exec_approval_request","call_id":"call_1","command":["ls"]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}
	var got []harnessproto.RuntimeEvent
	for _, l := range lines {
		got = append(got, MapCodexLine([]byte(l), st)...)
	}
	if len(got) != 3 || got[1].Type != TypePermissionResolved || got[2].Type != TypeTurnEnd {
		t.Fatalf("events = %+v, want request, resolved, turn_end", got)
	}
	if p := decodePermission(t, got[1]); p.Decision != harnessproto.DecisionCleared {
		t.Errorf("decision = %q, want %q", p.Decision, harnessproto.DecisionCleared)
	}
	// And the clear is not repeated on the next turn boundary.
	if extra := MapCodexLine([]byte(`{"type":"event_msg","payload":{"type":"turn_complete"}}`), st); len(extra) != 1 {
		t.Errorf("second turn end = %+v, want only the turn_end", extra)
	}
}

// TestTailMergesPermissionJournal is the streaming half of the contract: the
// journal is a second source of the same session's stream, so permission events
// arrive interleaved with the transcript's, under one ordinal space a consumer
// resumes with a single afterSeq.
func TestTailMergesPermissionJournal(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	journal := filepath.Join(dir, "perms.jsonl")
	write := func(path, line string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	write(transcript, `{"type":"user","message":{"role":"user","content":"delete the build dir"}}`)
	write(journal, `{"request_id":"perm-1","tool":"Bash","action":"rm -rf build/"}`)

	rec := Record{Runtime: harnessproto.RuntimeClaude, Path: transcript, Permissions: journal}
	stream := Stream(func(string) (Record, bool) { return rec, true }, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, ok := stream(ctx, "s1", 0)
	if !ok {
		t.Fatal("stream should start for a record with a journal")
	}

	var (
		types []string
		seqs  []int64
	)
	deadline := time.After(3 * time.Second)
	for len(types) < 4 {
		select {
		case b := <-ch:
			for _, ev := range b.Events {
				types = append(types, ev.Type)
			}
			seqs = append(seqs, b.Seq)
			if len(types) == 3 {
				// Only now resolve it, so the resolution has to arrive on a later poll
				// and take a later ordinal than everything before it.
				write(journal, `{"request_id":"perm-1","tool":"Bash","decision":"allow"}`)
			}
		case <-deadline:
			t.Fatalf("timed out after %v (seqs %v)", types, seqs)
		}
	}

	want := []string{TypeTurnStart, TypePrompt, TypePermissionRequest, TypePermissionResolved}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
	// One shared ordinal space: the last batch's seq counts every event so far, so
	// a consumer resuming at it skips the transcript and the journal alike.
	if seqs[len(seqs)-1] != 4 {
		t.Errorf("final seq = %d, want 4 (both sources share one ordinal space)", seqs[len(seqs)-1])
	}
}

// TestTailWithoutJournal keeps the single-source path honest: a record naming no
// journal streams exactly the transcript, and an unknown runtime still yields no
// stream at all.
func TestTailWithoutJournal(t *testing.T) {
	if _, ok := sourcesFor(Record{Runtime: "hermes", Path: "x"}); ok {
		t.Error("a runtime with no reader must not resolve to sources")
	}
	specs, ok := sourcesFor(Record{Runtime: harnessproto.RuntimeClaude, Path: "x"})
	if !ok || len(specs) != 1 || specs[0].permission {
		t.Errorf("claude without a journal = %+v, want just the transcript (no permission source)", specs)
	}
	specs, ok = sourcesFor(Record{Runtime: harnessproto.RuntimeCodex, Path: "x"})
	if !ok || len(specs) != 1 || !specs[0].permission {
		t.Errorf("codex = %+v, want one source that carries its own prompts", specs)
	}
}
