package runtimeevents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// noticeBody is the {level,text} a notice event carries.
type noticeBody struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

func decodeNotice(t *testing.T, ev harnessproto.RuntimeEvent) noticeBody {
	t.Helper()
	var b noticeBody
	if err := json.Unmarshal(ev.Payload, &b); err != nil {
		t.Fatalf("notice payload %s: %v", ev.Payload, err)
	}
	return b
}

// TestMapJournalLine covers the whole mapping: a record becomes a notice at its
// own level, a level-less record reads as info rather than as a blank level a
// consumer would have to guess at, and a line that isn't a record at all is kept
// as `raw` — the never-drop rule holds for amux's own format too.
func TestMapJournalLine(t *testing.T) {
	evs := MapJournalLine(harnessproto.RuntimeClaude,
		[]byte(`{"level":"error","text":"start agent a1: permission denied","at":1}`))
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeNotice {
		t.Fatalf("got %+v, want one notice", evs)
	}
	if evs[0].Direction != harnessproto.DirMeta {
		t.Errorf("direction = %q, want %q: a journal line is amux talking, not the agent",
			evs[0].Direction, harnessproto.DirMeta)
	}
	if got := decodeNotice(t, evs[0]); got.Level != core.JournalError ||
		got.Text != "start agent a1: permission denied" {
		t.Errorf("notice = %+v, want the error level and text verbatim", got)
	}

	evs = MapJournalLine(harnessproto.RuntimeClaude, []byte(`{"text":"starting agent"}`))
	if len(evs) != 1 {
		t.Fatalf("got %+v, want one notice", evs)
	}
	if got := decodeNotice(t, evs[0]); got.Level != core.JournalInfo {
		t.Errorf("level = %q, want %q for a record that named none", got.Level, core.JournalInfo)
	}

	if evs := MapJournalLine(harnessproto.RuntimeClaude, []byte("  \n")); evs != nil {
		t.Errorf("blank line produced %+v, want nothing", evs)
	}

	evs = MapJournalLine(harnessproto.RuntimeClaude, []byte(`{not json`))
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeRaw {
		t.Fatalf("unparsable line = %+v, want one raw event", evs)
	}
	if !strings.Contains(string(evs[0].Payload), "amux/journal-unparsable") {
		t.Errorf("raw payload %s should name the journal as its origin", evs[0].Payload)
	}
}

// TestStreamTailsTheJournalWithoutATranscript is the case the feature exists
// for: a session that has never run has no transcript, and the notices an
// accepted `prompt` writes while cold-starting it are the only thing there is to
// stream. A record with a journal and nothing else must still be streamable, or
// the caller that was told "accepted" sees silence.
func TestStreamTailsTheJournalWithoutATranscript(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "a1.jsonl")
	if err := os.WriteFile(journal,
		[]byte(`{"level":"info","text":"starting agent"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := Stream(func(string) (Record, bool) {
		return Record{Runtime: harnessproto.RuntimeClaude, Journal: journal}, true
	}, testPoll)
	ch, ok := src(ctx, "a1", 0)
	if !ok {
		t.Fatal("Stream ok=false for a record with a journal and no transcript")
	}

	// The failure the daemon reports after the ack arrives on the same stream.
	write(t, journal, `{"level":"error","text":"start agent a1: permission denied"}`+"\n")

	evs := collect(t, ch)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want the starting notice and the failure: %+v", len(evs), evs)
	}
	if got := decodeNotice(t, evs[0]); got.Text != "starting agent" || got.Level != core.JournalInfo {
		t.Errorf("first event = %+v, want the info notice", got)
	}
	if got := decodeNotice(t, evs[1]); got.Level != core.JournalError {
		t.Errorf("second event = %+v, want the error notice", got)
	}
}

// TestJournalIsOrderedLastPins the source order. Ordinals are shared across a
// session's sources and assigned in the order sourcesFor lists them, so adding
// the journal anywhere but last would renumber every event that predates it and
// make a resuming orchestrator's afterSeq cursor point at the wrong place.
func TestJournalIsOrderedLast(t *testing.T) {
	specs, ok := sourcesFor(Record{
		Runtime: harnessproto.RuntimeClaude, Path: "/t", Permissions: "/p", Journal: "/j",
	})
	if !ok || len(specs) != 3 {
		t.Fatalf("sourcesFor = %+v (ok=%v), want transcript, permissions, journal", specs, ok)
	}
	if specs[2].path != "/j" {
		t.Fatalf("source order = %q/%q/%q, want the journal last",
			specs[0].path, specs[1].path, specs[2].path)
	}
	if specs[2].permission {
		t.Error("the journal carries no permission events; replaying it for one is pure cost")
	}
}
