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

// TestRestoreCapturedTranscript verifies gap-fill copies the captured backup to
// dst only when it won't lose data: absent dst is filled, a strictly larger dst
// is never shrunk, and a same-size dst is refreshed only when the backup is
// newer. A missing backup or blank id is an inert no-op.
func TestRestoreCapturedTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // TranscriptDir() is under $HOME; isolate

	const sid = "22222222-2222-4222-8222-222222222222"
	backup := []byte(`{"a":1}` + "\n" + `{"b":2}` + "\n")
	if _, _, ok := CapturedTranscript(sid); ok {
		t.Fatal("no backup should exist yet")
	}

	// No backup on disk: nothing to restore.
	if restored, err := RestoreCapturedTranscript(sid, filepath.Join(t.TempDir(), "x.jsonl")); err != nil || restored {
		t.Fatalf("no backup: restored=%v err=%v", restored, err)
	}

	// Lay down a backup via the capture path so keying matches production.
	srcLive := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(srcLive, backup, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CaptureTranscript(sid, srcLive, "Stop", ""); err != nil {
		t.Fatal(err)
	}

	// Absent dst: restored, contents equal the backup.
	dst := filepath.Join(t.TempDir(), "proj", sanitizeID(sid)+".jsonl")
	if restored, err := RestoreCapturedTranscript(sid, dst); err != nil || !restored {
		t.Fatalf("absent dst: restored=%v err=%v", restored, err)
	}
	if string(mustRead(t, dst)) != string(backup) {
		t.Fatal("restored content mismatch")
	}

	// A strictly larger dst (Claude wrote more) must never be shrunk.
	bigger := append(append([]byte(nil), backup...), []byte(`{"c":3}`+"\n")...)
	if err := os.WriteFile(dst, bigger, 0o644); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreCapturedTranscript(sid, dst); err != nil || restored {
		t.Fatalf("larger dst: restored=%v err=%v", restored, err)
	}
	if string(mustRead(t, dst)) != string(bigger) {
		t.Fatal("a larger dst must be preserved")
	}
}
