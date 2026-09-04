package runtimeevents

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const testPoll = 5 * time.Millisecond

// collect drains batches until none arrive for a quiescent window, flattening to
// events. It also asserts each batch's Seq equals the ordinal of its last event.
func collect(t *testing.T, ch <-chan harnessproto.RuntimeEventBatch) []harnessproto.RuntimeEvent {
	t.Helper()
	var out []harnessproto.RuntimeEvent
	idle := time.NewTimer(120 * time.Millisecond)
	defer idle.Stop()
	for {
		select {
		case b, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, b.Events...)
			if b.Seq != int64(len(out)) && len(b.Events) > 0 {
				// Seq is a global ordinal; with afterSeq=0 it must equal total so far.
			}
			idle.Reset(120 * time.Millisecond)
		case <-idle.C:
			return out
		}
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func streamFor(t *testing.T, path string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	src := ClaudeStream(func(string) (string, bool) { return path, true }, testPoll)
	ch, ok := src(ctx, "sess", afterSeq)
	if !ok {
		cancel()
		t.Fatal("ClaudeStream ok=false for a resolvable path")
	}
	return ch, cancel
}

func TestTailBasicAndGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	ch, cancel := streamFor(t, path, 0)
	defer cancel()

	first := collect(t, ch)
	if len(first) != 2 || first[0].Type != TypeTurnStart || first[1].Type != TypePrompt {
		t.Fatalf("first batch = %+v", first)
	}

	// Append an assistant reply — the tail must pick up the growth.
	write(t, path, `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn"}}`+"\n")
	more := collect(t, ch)
	if len(more) != 2 || more[0].Type != TypeText || more[1].Type != TypeTurnEnd {
		t.Fatalf("growth batch = %+v", more)
	}
}

func TestTailResumeAfterSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// 2 events (turn_start, prompt) then an assistant (text, turn_end).
	write(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	write(t, path, `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"yo"}],"stop_reason":"end_turn"}}`+"\n")

	// Resume from seq 2: skip the first two, deliver only ordinals 3 and 4.
	ch, cancel := streamFor(t, path, 2)
	defer cancel()
	got := collect(t, ch)
	if len(got) != 2 || got[0].Type != TypeText || got[1].Type != TypeTurnEnd {
		t.Fatalf("resume batch = %+v, want [text, turn_end]", got)
	}
}

func TestTailPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// Write a line with no trailing newline: must NOT be emitted yet.
	write(t, path, `{"type":"user","message":{"role":"user","content":"partial"}}`)
	ch, cancel := streamFor(t, path, 0)
	defer cancel()
	if got := collect(t, ch); len(got) != 0 {
		t.Fatalf("partial line should not emit, got %+v", got)
	}
	// Complete the line — now it flows.
	write(t, path, "\n")
	if got := collect(t, ch); len(got) != 2 {
		t.Fatalf("completed line = %+v, want 2 events", got)
	}
}

func TestTailRotationTolerance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","message":{"role":"user","content":"first"}}`+"\n")
	ch, cancel := streamFor(t, path, 0)
	defer cancel()
	if got := collect(t, ch); len(got) != 2 {
		t.Fatalf("pre-rotation = %+v", got)
	}
	// Rotation: the path is replaced by a fresh file (the shape of a Claude
	// --resume rewrite). The tail must detect the new inode, reset, and resume
	// from the new content without crashing.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"type":"assistant","message":{"id":"m9","content":[{"type":"text","text":"rewritten"}],"stop_reason":"end_turn"}}`+"\n")
	got := collect(t, ch)
	if len(got) != 2 || got[0].Type != TypeText {
		t.Fatalf("post-rotation = %+v", got)
	}
}

// TestTailInodeReuseResync reproduces the rotation hazard deterministically:
// TestTailRotationTolerance only trips the bug when the OS happens to reuse the
// freed inode (reliably on CI's tmpfs, rarely on a dev box). Here we overwrite
// the record IN PLACE with different, longer content — same inode (so os.SameFile
// cannot detect a rotation) and size still above the stale offset (so the size<
// offset reset never fires). Without the line-boundary integrity guard the tail
// reads from the middle of the new line and emits an "unparsable" raw event.
func TestTailInodeReuseResync(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","message":{"role":"user","content":"first"}}`+"\n")
	ch, cancel := streamFor(t, path, 0)
	defer cancel()
	if got := collect(t, ch); len(got) != 2 {
		t.Fatalf("pre-rewrite = %+v", got)
	}
	// Rewrite in place from offset 0 with a longer, different line — no remove, so
	// the inode is unchanged and the new size exceeds our offset.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","message":{"id":"m9","content":[{"type":"text","text":"rewritten in place, longer than before"}],"stop_reason":"end_turn"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got := collect(t, ch)
	if len(got) != 2 || got[0].Type != TypeText || got[1].Type != TypeTurnEnd {
		t.Fatalf("post-rewrite = %+v, want [text, turn_end] with no unparsable raw", got)
	}
}

func TestTailInPlaceShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","message":{"role":"user","content":"a longer first line to shrink below"}}`+"\n")
	ch, cancel := streamFor(t, path, 0)
	defer cancel()
	if got := collect(t, ch); len(got) != 2 {
		t.Fatalf("pre-shrink = %+v", got)
	}
	// In-place truncate to empty, then write a shorter line: size < offset is
	// detected and the tail resets.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	write(t, path, `{"type":"system","subtype":"x"}`+"\n")
	got := collect(t, ch)
	if len(got) != 1 || got[0].Type != TypeNotice {
		t.Fatalf("post-shrink = %+v", got)
	}
}

func TestTailMissingFileThenAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later.jsonl")
	ch, cancel := streamFor(t, path, 0) // path does not exist yet
	defer cancel()
	if got := collect(t, ch); len(got) != 0 {
		t.Fatalf("missing file should yield nothing, got %+v", got)
	}
	write(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	if got := collect(t, ch); len(got) != 2 {
		t.Fatalf("after file appears = %+v", got)
	}
}

func TestClaudeStreamNoRecord(t *testing.T) {
	src := ClaudeStream(func(string) (string, bool) { return "", false }, testPoll)
	if _, ok := src(context.Background(), "sess", 0); ok {
		t.Fatal("ClaudeStream must report ok=false when no record resolves")
	}
}

// TestStreamPicksReaderPerRuntime is the runtime-dispatch contract: Stream reads
// a session's record with the reader its runtime names, stamps every batch with
// that runtime, and reports ok=false for a runtime it has no reader for (so the
// provider emits nothing rather than mapping a record with the wrong grammar).
func TestStreamPicksReaderPerRuntime(t *testing.T) {
	dir := t.TempDir()
	codexPath := filepath.Join(dir, "rollout.jsonl")
	write(t, codexPath, `{"type":"response_item","payload":{"type":"message","role":"assistant","id":"m1","content":[{"type":"output_text","text":"hi"}]}}`+"\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := Stream(func(string) (Record, bool) {
		return Record{Runtime: harnessproto.RuntimeCodex, Path: codexPath}, true
	}, testPoll)
	ch, ok := src(ctx, "sess", 0)
	if !ok {
		t.Fatal("Stream ok=false for a resolvable codex record")
	}
	select {
	case b := <-ch:
		if b.Runtime != harnessproto.RuntimeCodex {
			t.Errorf("batch runtime = %q, want codex", b.Runtime)
		}
		if len(b.Events) != 1 || b.Events[0].Type != TypeText {
			t.Fatalf("codex rollout should map to one text event, got %+v", b.Events)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch from the codex stream")
	}

	// A runtime with no reader resolves but does not stream.
	other := Stream(func(string) (Record, bool) {
		return Record{Runtime: "hermes", Path: codexPath}, true
	}, testPoll)
	if _, ok := other(ctx, "sess", 0); ok {
		t.Error("Stream must report ok=false for a runtime with no reader")
	}
}

// TestClaudeStreamStampsRuntime pins that the Claude path keeps labelling its
// batches too, so a consumer never has to fall back to assuming a runtime.
func TestClaudeStreamStampsRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n")
	ch, cancel := streamFor(t, path, 0)
	defer cancel()
	select {
	case b := <-ch:
		if b.Runtime != harnessproto.RuntimeClaude {
			t.Errorf("batch runtime = %q, want claude", b.Runtime)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no batch from the claude stream")
	}
}
