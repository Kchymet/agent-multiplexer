package runtimeevents

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Benchmarks over a synthetic ~10 MB Codex rollout. BenchmarkFullReplay is what
// every subscription (at any afterSeq) and every OpenPermissions call cost before
// the index; the others are their indexed replacements once the index is warm.

func benchRecord(b *testing.B) Record {
	b.Helper()
	path := bigRecord(b, "testdata/codex_session.jsonl", 1000)
	return Record{Runtime: harnessproto.RuntimeCodex, Path: path}
}

func BenchmarkFullReplay(b *testing.B) {
	rec := benchRecord(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		replayAll(rec.Path, PageSourceCodex, rec.Runtime)
	}
}

func BenchmarkHistoryNewestPage(b *testing.B) {
	rec := benchRecord(b)
	if _, err := History(rec, 0, 0); err != nil { // cold build, outside the timer
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := History(rec, 0, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoryOldestPage(b *testing.B) {
	rec := benchRecord(b)
	if _, err := History(rec, 0, 0); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := History(rec, DefaultHistoryLimit+1, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenPermissionsIndexed(b *testing.B) {
	rec := benchRecord(b)
	OpenPermissions(rec)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		OpenPermissions(rec)
	}
}

// TestLargeTranscriptCost reports before/after costs for a real transcript:
//
//	AMUX_BENCH_TRANSCRIPT=/path/to/rollout.jsonl AMUX_BENCH_RUNTIME=codex \
//	  go test ./internal/runtimeevents -run LargeTranscriptCost -v
//
// "before" is the full decode a subscription or permission lookup used to do
// and the single frame a from-zero subscription used to send; "after" is the
// indexed newest page the web UI now opens with.
func TestLargeTranscriptCost(t *testing.T) {
	path := os.Getenv("AMUX_BENCH_TRANSCRIPT")
	if path == "" {
		t.Skip("set AMUX_BENCH_TRANSCRIPT to a transcript path")
	}
	runtime := os.Getenv("AMUX_BENCH_RUNTIME")
	if runtime == "" {
		runtime = harnessproto.RuntimeClaude
	}
	rec := Record{Runtime: runtime, Path: path}
	specs, ok := sourcesFor(rec)
	if !ok {
		t.Fatalf("no reader for runtime %q", runtime)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	all := replayAll(path, specs[0].kind, runtime)
	replay := time.Since(start)
	full, _ := json.Marshal(harnessproto.HarnessMsg{Type: harnessproto.HRuntimeEvents, SessionID: "s", Runtime: runtime, Seq: int64(len(all)), Events: all})
	t.Logf("transcript: %d bytes, %d events", fi.Size(), len(all))
	t.Logf("before: full decode per subscribe/permission lookup %v; from-zero frame %d bytes", replay, len(full))

	start = time.Now()
	page, err := History(rec, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	start = time.Now()
	page, _ = History(rec, 0, 0)
	warm := time.Since(start)
	frame, _ := json.Marshal(harnessproto.HarnessMsg{Type: harnessproto.HRuntimeEventsPage, SessionID: "s", Runtime: runtime,
		Seq: page.Seq, HeadSeq: page.Head, HasMore: page.HasMore, Events: page.Events})
	start = time.Now()
	History(rec, DefaultHistoryLimit+1, 0)
	oldest := time.Since(start)
	start = time.Now()
	OpenPermissions(rec)
	perms := time.Since(start)
	ctx, cancel := context.WithCancel(context.Background())
	start = time.Now()
	ch, _ := Stream(func(string) (Record, bool) { return rec, true }, time.Hour)(ctx, "s", page.Head-50)
	<-ch
	resume := time.Since(start)
	cancel()
	t.Logf("after: resume subscription at head-50 %v", resume)
	t.Logf("after: newest %d-event page %d bytes; cold (index build) %v, warm %v; oldest page %v; OpenPermissions warm %v",
		len(page.Events), len(frame), cold, warm, oldest, perms)
}
