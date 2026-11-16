package runtimeevents

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const claudeTurnEnd = `{"type":"assistant","message":{"id":"msg_%s","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func turnEndIndexes(src SourceEvents) []int64 {
	var out []int64
	for i, ev := range src.Events {
		if ev.Type == TypeTurnEnd {
			out = append(out, src.First+int64(i))
		}
	}
	return out
}

func source(p SourcesPage, kind PageSourceKind) SourceEvents {
	for _, s := range p.Sources {
		if s.Kind == string(kind) {
			return s
		}
	}
	return SourceEvents{}
}

// A transcript append after the journal already has events shifts every
// journal event's joined ordinal, but per-source indexes never move, so a
// per-source cursor sees the new transcript turn end exactly once and nothing
// in the journal is replayed.
func TestReadSourcesIndexesAreStableAcrossSourceGrowth(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "t.jsonl")
	journal := filepath.Join(dir, "j.jsonl")
	appendFile(t, transcript, sprintf(claudeTurnEnd, "1"))
	appendFile(t, journal, `{"level":"info","text":"starting agent"}`+"\n"+`{"level":"info","text":"ready"}`+"\n")
	rec := Record{Runtime: harnessproto.RuntimeClaude, Path: transcript, Journal: journal}

	all, err := ReadSources(rec, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	tr, jr := source(all, PageSourceClaude), source(all, PageSourceJournal)
	cursor := map[string]int64{string(PageSourceClaude): tr.Head, string(PageSourceJournal): jr.Head}
	journalFirst := jr.Events[0]

	// Joined ordinals before the append.
	h1, _ := History(rec, 0, 100)

	appendFile(t, transcript, sprintf(claudeTurnEnd, "2"))
	next, err := ReadSources(rec, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := turnEndIndexes(source(next, PageSourceClaude)); len(got) != 1 || got[0] <= tr.Head {
		t.Fatalf("new transcript turn end indexes = %v (transcript head was %d)", got, tr.Head)
	}
	if n := len(source(next, PageSourceJournal).Events); n != 0 {
		t.Fatalf("journal replayed %d events after a transcript append", n)
	}
	// The joined ordinal space shifted under the journal (why a joined cursor
	// is not a stable identity).
	h2, _ := History(rec, 0, 100)
	if h2.Head <= h1.Head {
		t.Fatalf("history head did not grow: %d -> %d", h1.Head, h2.Head)
	}
	// The journal's first event keeps its per-source index 1.
	again, _ := ReadSources(rec, map[string]int64{string(PageSourceJournal): 0}, 1)
	if j := source(again, PageSourceJournal); j.First != 1 || len(j.Events) != 1 || j.Events[0].Type != journalFirst.Type {
		t.Fatalf("journal index 1 moved: %+v", j)
	}
}

func TestReadSourcesMissingSourceAndBounds(t *testing.T) {
	dir := t.TempDir()
	journal := filepath.Join(dir, "j.jsonl")
	appendFile(t, journal, `{"level":"info","text":"a"}`+"\n"+`{"level":"info","text":"b"}`+"\n"+`{"level":"info","text":"c"}`+"\n")
	rec := Record{Runtime: harnessproto.RuntimeClaude, Path: filepath.Join(dir, "absent.jsonl"), Journal: journal}
	p, err := ReadSources(rec, map[string]int64{string(PageSourceJournal): 1}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c := source(p, PageSourceClaude); c.Head != 0 || len(c.Events) != 0 {
		t.Fatalf("missing transcript = %+v", c)
	}
	if j := source(p, PageSourceJournal); j.Head != 3 || j.First != 2 || len(j.Events) != 1 {
		t.Fatalf("bounded journal read = %+v", j)
	}
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
