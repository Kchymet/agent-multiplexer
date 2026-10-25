package runtimeevents

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// bigRecord writes copies of a fixture into one file, large enough to span many
// index checkpoints, and returns its path.
func bigRecord(t testing.TB, fixture string, copies int) string {
	t.Helper()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "record.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat(string(body), copies)), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// replayAll is the reference the index must agree with: every complete line of
// the file decoded from the top with one fresh decoder.
func replayAll(path string, kind PageSourceKind, runtime string) []harnessproto.RuntimeEvent {
	return mapEachLine(path, newSourceDecoder(kind, runtime).mapLine)
}

func updateIndex(t *testing.T, ix *sourceIndex, path string) indexSnapshot {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	snap, err := ix.update(f)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func readIndexRange(t *testing.T, ix *sourceIndex, path string, from, to int64) []harnessproto.RuntimeEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := ix.readRange(f, from, to)
	if err != nil {
		t.Fatalf("readRange(%d,%d): %v", from, to, err)
	}
	return got
}

func TestIndexRangesMatchFullReplay(t *testing.T) {
	path := bigRecord(t, "testdata/codex_session.jsonl", 120) // ~1.3 MB, several checkpoints
	want := replayAll(path, PageSourceCodex, harnessproto.RuntimeCodex)
	ix := newSourceIndex(PageSourceCodex, harnessproto.RuntimeCodex)
	snap := updateIndex(t, ix, path)
	if snap.events != int64(len(want)) {
		t.Fatalf("indexed %d events, replay has %d", snap.events, len(want))
	}
	if len(ix.cps) < 4 {
		t.Fatalf("expected several checkpoints over %d bytes, got %d", snap.end, len(ix.cps))
	}
	n := int64(len(want))
	// Windows at the very start and end, across checkpoint boundaries, and singletons.
	windows := [][2]int64{{0, 1}, {0, 200}, {n - 1, n}, {n - 200, n}, {0, n}}
	for _, cp := range ix.cps[1:] {
		windows = append(windows, [2]int64{cp.events - 3, cp.events + 3}, [2]int64{cp.events, cp.events + 1})
	}
	for _, w := range windows {
		got := readIndexRange(t, ix, path, w[0], w[1])
		if !reflect.DeepEqual(got, want[w[0]:w[1]]) {
			t.Fatalf("range [%d,%d) differs from full replay", w[0], w[1])
		}
	}
}

func TestIndexPartialTrailingLineAndIncrementalAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, userLine("one"))
	write(t, path, `{"type":"user","message":{"role":"user","content":"tw`) // no newline yet
	ix := newSourceIndex(PageSourceClaude, harnessproto.RuntimeClaude)
	snap := updateIndex(t, ix, path)
	first := int64(len(userLine("one")))
	if snap.end != first || snap.events != 2 { // turn_start + prompt
		t.Fatalf("partial line was indexed: end=%d events=%d", snap.end, snap.events)
	}
	scanned := ix.scanned

	write(t, path, `o"}}`+"\n")
	snap = updateIndex(t, ix, path)
	if snap.events != 4 {
		t.Fatalf("completed line not indexed: events=%d", snap.events)
	}
	if got := ix.scanned - scanned; got != snap.end-first {
		t.Fatalf("append rescanned %d bytes, want only the %d-byte tail", got, snap.end-first)
	}
	// An unchanged file costs no decode at all.
	scanned = ix.scanned
	updateIndex(t, ix, path)
	if ix.scanned != scanned {
		t.Fatal("unchanged file was rescanned")
	}
	if got := readIndexRange(t, ix, path, 2, 4); !reflect.DeepEqual(got, replayAll(path, PageSourceClaude, harnessproto.RuntimeClaude)[2:4]) {
		t.Fatalf("appended events differ: %+v", got)
	}
}

func TestIndexInvalidatesOnTruncationRotationAndRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	write(t, path, userLine("aaaa")+userLine("bbbb")+userLine("cccc"))
	ix := newSourceIndex(PageSourceClaude, harnessproto.RuntimeClaude)
	if snap := updateIndex(t, ix, path); snap.events != 6 {
		t.Fatalf("events=%d", snap.events)
	}
	check := func(name string) {
		t.Helper()
		want := replayAll(path, PageSourceClaude, harnessproto.RuntimeClaude)
		snap := updateIndex(t, ix, path)
		if snap.events != int64(len(want)) {
			t.Fatalf("%s: index has %d events, file has %d", name, snap.events, len(want))
		}
		if got := readIndexRange(t, ix, path, 0, snap.events); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: stale content served", name)
		}
	}

	// Truncation below the indexed end.
	if err := os.Truncate(path, int64(len(userLine("aaaa")))); err != nil {
		t.Fatal(err)
	}
	check("truncate")

	// Rotation: a different file renamed over the path.
	next := filepath.Join(dir, "next.jsonl")
	write(t, next, userLine("rotated")+userLine("again"))
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	check("rotate")

	// In-place rewrite to the same size (same inode, size unchanged): the anchor
	// before the indexed end no longer matches.
	body, _ := os.ReadFile(path)
	rewritten := strings.Replace(string(body), "again", "AGAIN", 1)
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, future, future)
	check("rewrite")
}

func TestHistoryPagesBackToTheStart(t *testing.T) {
	dir := t.TempDir()
	path := bigRecord(t, "testdata/codex_session.jsonl", 40)
	journal := filepath.Join(dir, "journal.jsonl")
	write(t, journal, `{"level":"info","text":"starting agent"}`+"\n"+`{"level":"error","text":"boom"}`+"\n")
	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: path, Journal: journal}

	// The reference ordinal space is a fresh subscription's: everything streamed
	// from afterSeq 0.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, ok := Stream(func(string) (Record, bool) { return rec, true }, time.Hour)(ctx, "s", 0)
	if !ok {
		t.Fatal("stream refused")
	}
	var want []harnessproto.RuntimeEvent
	for len(want) == 0 || int64(len(want)) < int64(len(replayAll(path, PageSourceCodex, harnessproto.RuntimeCodex)))+2 {
		b := <-ch
		if b.Seq != int64(len(want)+len(b.Events)) {
			t.Fatalf("frame seq %d does not end at ordinal %d", b.Seq, len(want)+len(b.Events))
		}
		if len(b.Events) > maxBatchEvents {
			t.Fatalf("replay frame of %d events exceeds the %d cap", len(b.Events), maxBatchEvents)
		}
		want = append(want, b.Events...)
	}
	cancel()

	newest, err := History(rec, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	head := int64(len(want))
	if newest.Head != head || newest.Seq != head || len(newest.Events) != DefaultHistoryLimit || !newest.HasMore {
		t.Fatalf("newest page: head=%d seq=%d len=%d more=%v (want head %d)", newest.Head, newest.Seq, len(newest.Events), newest.HasMore, head)
	}
	if newest.Runtime != harnessproto.RuntimeCodex {
		t.Fatalf("runtime %q", newest.Runtime)
	}

	// Walk back with odd page sizes; the pages reassemble the exact stream.
	var got []harnessproto.RuntimeEvent
	before := int64(0)
	pages := 0
	for {
		page, err := History(rec, before, 37)
		if err != nil {
			t.Fatal(err)
		}
		first := page.Seq - int64(len(page.Events)) + 1
		got = append(append([]harnessproto.RuntimeEvent(nil), page.Events...), got...)
		pages++
		if !page.HasMore {
			if first != 1 {
				t.Fatalf("last page starts at %d, not 1", first)
			}
			break
		}
		before = first
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged history (%d events over %d pages) differs from the stream (%d events)", len(got), pages, len(want))
	}

	// Boundaries.
	if p, _ := History(rec, 1, 10); len(p.Events) != 0 || p.HasMore || p.Seq != 0 || p.Head != head {
		t.Fatalf("beforeSeq=1: %+v", p)
	}
	if p, _ := History(rec, 2, 10); len(p.Events) != 1 || p.HasMore || p.Seq != 1 {
		t.Fatalf("beforeSeq=2: seq=%d len=%d more=%v", p.Seq, len(p.Events), p.HasMore)
	}
	if p, _ := History(rec, head+100, 5); p.Seq != head || len(p.Events) != 5 {
		t.Fatalf("beforeSeq past head is not clamped: seq=%d len=%d", p.Seq, len(p.Events))
	}
	if p, _ := History(rec, 0, MaxHistoryLimit*10); len(p.Events) != int(min(head, MaxHistoryLimit)) {
		t.Fatalf("limit not clamped: %d", len(p.Events))
	}
	bigger := Record{Runtime: harnessproto.RuntimeCodex, Path: bigRecord(t, "testdata/codex_session.jsonl", 100)}
	if p, _ := History(bigger, 0, MaxHistoryLimit*10); len(p.Events) != MaxHistoryLimit || !p.HasMore {
		t.Fatalf("limit not clamped: %d (head %d)", len(p.Events), p.Head)
	}
	// The page spanning the transcript/journal boundary keeps both sources' order.
	if p, _ := History(rec, head+1, 3); !reflect.DeepEqual(p.Events, want[head-3:]) {
		t.Fatal("cross-source page differs")
	}
}

func TestHistoryEmptyAndMissingRecord(t *testing.T) {
	rec := Record{Runtime: harnessproto.RuntimeClaude, Path: filepath.Join(t.TempDir(), "absent.jsonl")}
	p, err := History(rec, 0, 0)
	if err != nil || len(p.Events) != 0 || p.Head != 0 || p.HasMore {
		t.Fatalf("missing record: %+v %v", p, err)
	}
	if _, err := History(Record{Runtime: "unknown", Path: rec.Path}, 0, 0); err == nil {
		t.Fatal("a runtime with no reader must be refused")
	}
}

// The live handoff: subscribing at the newest page's head delivers exactly what
// is appended afterwards, numbered from head+1.
func TestHistoryHeadThenSubscribeHasNoGapOrOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, userLine("one")+userLine("two"))
	rec := Record{Runtime: harnessproto.RuntimeClaude, Path: path}
	page, err := History(rec, 0, 0)
	if err != nil || page.Head != 4 {
		t.Fatalf("head=%d err=%v", page.Head, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := Stream(func(string) (Record, bool) { return rec, true }, testPoll)(ctx, "s", page.Head)
	select {
	case b := <-ch:
		t.Fatalf("history already delivered, stream re-sent %d events", len(b.Events))
	case <-time.After(50 * time.Millisecond):
	}
	write(t, path, userLine("three"))
	b := <-ch
	if b.Seq != 6 || len(b.Events) != 2 || b.Events[1].Type != TypePrompt {
		t.Fatalf("live frame seq=%d events=%+v", b.Seq, b.Events)
	}
}

// Resuming from any afterSeq yields the same suffix a full replay would, at
// every position relative to the checkpoints.
func TestStreamResumeFromCheckpointMatchesFullReplay(t *testing.T) {
	path := bigRecord(t, "testdata/codex_session.jsonl", 80)
	want := replayAll(path, PageSourceCodex, harnessproto.RuntimeCodex)
	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: path}
	ix := indexFor(sourceSpec{path: path, kind: PageSourceCodex, runtime: harnessproto.RuntimeCodex})
	updateIndex(t, ix, path)
	afters := []int64{1, int64(len(want)) - 1, int64(len(want))}
	for _, cp := range ix.cps[1:] {
		afters = append(afters, cp.events-1, cp.events, cp.events+1)
	}
	for _, after := range afters {
		ctx, cancel := context.WithCancel(context.Background())
		ch, _ := Stream(func(string) (Record, bool) { return rec, true }, time.Hour)(ctx, "s", after)
		var got []harnessproto.RuntimeEvent
		for int64(len(got)) < int64(len(want))-after {
			b := <-ch
			if b.Seq != after+int64(len(got)+len(b.Events)) {
				t.Fatalf("after=%d: frame seq %d", after, b.Seq)
			}
			got = append(got, b.Events...)
		}
		cancel()
		if len(got) != len(want[after:]) || (len(got) > 0 && !reflect.DeepEqual(got, want[after:])) {
			t.Fatalf("after=%d: resumed stream differs from replay suffix", after)
		}
	}
}

func TestOpenPermissionsFromIndexTracksAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	body, err := os.ReadFile("testdata/codex_approvals.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(body), "\n")
	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: path}
	// Grow the rollout one line at a time; after every append the index's open
	// set equals a from-the-top replay of the same prefix.
	sawOpen := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		write(t, path, line)
		var want []Pending
		want = applyPending(want, replayAll(path, PageSourceCodex, harnessproto.RuntimeCodex))
		if got := OpenPermissions(rec); len(got) != len(want) || (len(got) > 0 && !reflect.DeepEqual(got, want)) {
			t.Fatalf("after %q: open=%+v want %+v", strings.TrimSpace(line), got, want)
		}
		sawOpen = sawOpen || len(want) > 0
	}
	if !sawOpen {
		t.Fatal("fixture never had an approval open; the test checks nothing")
	}
}

func TestSplitBatchKeepsOrdinals(t *testing.T) {
	events := make([]harnessproto.RuntimeEvent, maxBatchEvents*2+5)
	for i := range events {
		events[i] = harnessproto.RuntimeEvent{Type: TypeText, Payload: []byte(`{}`)}
	}
	frames := splitBatch(harnessproto.RuntimeEventBatch{Seq: 1000 + int64(len(events)), Events: events})
	if len(frames) != 3 {
		t.Fatalf("%d frames", len(frames))
	}
	next := int64(1001)
	for _, f := range frames {
		if f.Seq != next+int64(len(f.Events))-1 {
			t.Fatalf("frame seq %d, want %d", f.Seq, next+int64(len(f.Events))-1)
		}
		next = f.Seq + 1
	}
	big := []harnessproto.RuntimeEvent{{Payload: make([]byte, maxBatchPayloadBytes)}, {Payload: make([]byte, 10)}}
	if frames := splitBatch(harnessproto.RuntimeEventBatch{Seq: 2, Events: big}); len(frames) != 2 || frames[0].Seq != 1 {
		t.Fatalf("oversized event not framed alone: %+v", frames)
	}
}

// userLine is one Claude user record: it maps to turn_start + prompt.
func userLine(text string) string {
	return `{"type":"user","message":{"role":"user","content":"` + text + `"}}` + "\n"
}
