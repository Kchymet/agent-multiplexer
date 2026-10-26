package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureFile stages and publishes one capture of path as a hook would.
func captureFile(t *testing.T, path, event string) *SessionTranscriptStage {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stage, err := StageSessionTranscript(context.Background(), "a1", "r1", event, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Publish(); err != nil {
		t.Fatal(err)
	}
	return stage
}

func capturedContent(t *testing.T) string {
	t.Helper()
	path, _, ok := SessionCapturedTranscript("a1", "r1")
	if !ok {
		t.Fatal("capture not published")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSessionTranscriptCaptureAppendsOnlyNewBytes pins delta capture: an
// append-only transcript is copied in full once, then only its growth is
// staged, while truncation, in-place rewrite, replacement and a damaged copy
// each fall back to a full, exact copy.
func TestSessionTranscriptCaptureAppendsOnlyNewBytes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "t.jsonl")
	line := strings.Repeat("x", 100) + "\n"
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	appendLine := func(content string) {
		t.Helper()
		f, err := os.OpenFile(src, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(content); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	want := strings.Repeat(line, 100)
	write(want)
	if stage := captureFile(t, src, "UserPromptSubmit"); stage.base != 0 {
		t.Fatalf("first capture staged a delta from %d", stage.base)
	}

	appendLine("{\"next\":1}\n")
	want += "{\"next\":1}\n"
	stage := captureFile(t, src, "PostToolUse")
	if stage.base != int64(len(want))-11 || stage.durable {
		t.Fatalf("append staged base %d durable %v, want delta from %d without fsync", stage.base, stage.durable, len(want)-11)
	}
	if got := capturedContent(t); got != want {
		t.Fatalf("delta capture = %d bytes, want %d", len(got), len(want))
	}
	if stage := captureFile(t, src, "Stop"); stage.base != int64(len(want)) || !stage.durable {
		t.Fatalf("unchanged turn-end capture base %d durable %v", stage.base, stage.durable)
	}

	// Truncation (e.g. /clear rewriting the file) starts over.
	want = "{\"short\":1}\n"
	write(want)
	if stage := captureFile(t, src, "PostToolUse"); stage.base != 0 {
		t.Fatalf("truncated source appended from %d", stage.base)
	}
	if got := capturedContent(t); got != want {
		t.Fatalf("capture after truncation = %q", got)
	}

	// Same size or longer but rewritten in place: the tail anchor differs.
	want = "{\"other\":1}\n" + strings.Repeat(line, 3)
	write(want)
	if stage := captureFile(t, src, "PostToolUse"); stage.base != 0 {
		t.Fatalf("rewritten source appended from %d", stage.base)
	}
	if got := capturedContent(t); got != want {
		t.Fatalf("capture after rewrite = %q", got)
	}

	// Replaced by a new file (new inode) with identical leading bytes.
	replacement := src + ".new"
	want += "{\"more\":1}\n"
	if err := os.WriteFile(replacement, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, src); err != nil {
		t.Fatal(err)
	}
	if stage := captureFile(t, src, "PostToolUse"); stage.base != 0 {
		t.Fatalf("replaced source appended from %d", stage.base)
	}
	if got := capturedContent(t); got != want {
		t.Fatalf("capture after replacement = %q", got)
	}

	// A published copy whose size no longer matches its metadata (a crash
	// between append and metadata) is re-copied, never appended to.
	path, _, _ := SessionCapturedTranscript("a1", "r1")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("partial")
	_ = f.Close()
	appendLine("{\"after\":1}\n")
	want += "{\"after\":1}\n"
	if stage := captureFile(t, src, "PostToolUse"); stage.base != 0 {
		t.Fatalf("damaged copy appended from %d", stage.base)
	}
	if got := capturedContent(t); got != want {
		t.Fatalf("capture after damaged copy = %q", got)
	}
}

func BenchmarkSessionTranscriptCapture(b *testing.B) {
	b.Setenv("HOME", b.TempDir())
	src := filepath.Join(b.TempDir(), "t.jsonl")
	if err := os.WriteFile(src, []byte(strings.Repeat(strings.Repeat("x", 1023)+"\n", 32<<10)), 0o600); err != nil {
		b.Fatal(err) // 32 MiB, a long-lived coordinator's transcript
	}
	capture := func(b *testing.B) {
		f, err := os.Open(src)
		if err != nil {
			b.Fatal(err)
		}
		stage, err := StageSessionTranscript(context.Background(), "a1", "r1", "PostToolUse", f)
		if err == nil {
			err = stage.Publish()
		}
		_ = f.Close()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Run("delta", func(b *testing.B) {
		capture(b)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			f, _ := os.OpenFile(src, os.O_WRONLY|os.O_APPEND, 0)
			_, _ = f.WriteString("{\"tool\":\"result\"}\n")
			_ = f.Close()
			capture(b)
		}
	})
	b.Run("full", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, metadata, _ := sessionTranscriptPaths("a1", "r1")
			_ = os.Remove(metadata) // force the pre-delta behavior
			capture(b)
		}
	})
}
