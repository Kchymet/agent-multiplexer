package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureTranscript verifies a present transcript is copied verbatim and the
// event is logged, that a missing transcript logs present=false without copying,
// and that a blank id/src is an inert no-op.
func TestCaptureTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // StateDir() is under $HOME; isolate the test

	src := filepath.Join(t.TempDir(), "live.jsonl")
	content := []byte(`{"role":"user"}` + "\n" + `{"role":"assistant"}` + "\n")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	const sid = "11111111-1111-4111-8111-111111111111"
	if err := CaptureTranscript(sid, src, "Stop", "abc123"); err != nil {
		t.Fatalf("capture: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(TranscriptDir(), sanitizeID(sid)+".jsonl"))
	if err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("snapshot mismatch: got %q want %q", got, content)
	}

	// The log records the event and that the transcript was present.
	logB, err := os.ReadFile(filepath.Join(TranscriptDir(), sanitizeID(sid)+".log"))
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}
	var rec captureLogLine
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(logB))), &rec); err != nil {
		t.Fatalf("log line not JSON: %v", err)
	}
	if !rec.Present || rec.Event != "Stop" || rec.AmuxID != "abc123" || rec.Bytes != int64(len(content)) {
		t.Fatalf("unexpected log line: %+v", rec)
	}

	// A missing transcript logs the absence and does not clobber the last snapshot.
	if err := CaptureTranscript(sid, filepath.Join(t.TempDir(), "gone.jsonl"), "PreCompact", ""); err != nil {
		t.Fatalf("capture(missing): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(mustRead(t, filepath.Join(TranscriptDir(), sanitizeID(sid)+".log")))), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 log lines, got %d", len(lines))
	}
	var rec2 captureLogLine
	_ = json.Unmarshal([]byte(lines[1]), &rec2)
	if rec2.Present {
		t.Fatal("missing transcript should log present=false")
	}
	if string(mustRead(t, filepath.Join(TranscriptDir(), sanitizeID(sid)+".jsonl"))) != string(content) {
		t.Fatal("a missing transcript must not overwrite the previous snapshot")
	}

	// Blank id or src is a no-op (no error, nothing to assert beyond not panicking).
	if err := CaptureTranscript("", src, "Stop", ""); err != nil {
		t.Fatalf("blank id: %v", err)
	}
	if err := CaptureTranscript(sid, "", "Stop", ""); err != nil {
		t.Fatalf("blank src: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
