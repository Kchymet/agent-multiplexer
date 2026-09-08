package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionTranscriptScopesEqualRuntimeIDsBySubject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const runtimeID = "same-runtime-id"
	if _, err := CaptureSessionTranscript("subject-a", runtimeID, "Stop", strings.NewReader("a\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureSessionTranscript("subject-b", runtimeID, "Stop", strings.NewReader("b\n")); err != nil {
		t.Fatal(err)
	}
	a, _, okA := SessionCapturedTranscript("subject-a", runtimeID)
	b, _, okB := SessionCapturedTranscript("subject-b", runtimeID)
	if !okA || !okB || a == b {
		t.Fatalf("scoped captures a=%q/%v b=%q/%v", a, okA, b, okB)
	}
	dataA, _ := os.ReadFile(a)
	dataB, _ := os.ReadFile(b)
	if string(dataA) != "a\n" || string(dataB) != "b\n" {
		t.Fatalf("capture contents a=%q b=%q", dataA, dataB)
	}
	if _, _, ok := SessionCapturedTranscript("foreign", runtimeID); ok {
		t.Fatal("foreign subject restored another subject's equal runtime id")
	}
}

func TestSessionTranscriptPublishFailureRestoresPreviousCapture(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const (
		subject   = "subject"
		runtimeID = "runtime"
	)
	if _, err := CaptureSessionTranscript(subject, runtimeID, "Stop", strings.NewReader("previous\n")); err != nil {
		t.Fatal(err)
	}
	stage, err := StageSessionTranscript(context.Background(), subject, runtimeID, "Stop", strings.NewReader("replacement\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Abort()

	originalWrite := writeSessionTranscriptMeta
	writeSessionTranscriptMeta = func(string, []byte) error { return errors.New("synthetic metadata failure") }
	t.Cleanup(func() { writeSessionTranscriptMeta = originalWrite })
	if err := stage.Publish(); err == nil {
		t.Fatal("publish succeeded despite metadata failure")
	}

	path, _, ok := SessionCapturedTranscript(subject, runtimeID)
	if !ok {
		t.Fatal("previous capture was lost after failed replacement")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "previous\n" {
		t.Fatalf("capture after failed replacement = %q, %v", data, err)
	}
}

type pacedReader struct {
	once    sync.Once
	started chan struct{}
	left    int
}

func (r *pacedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	if r.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(5 * time.Millisecond)
	p[0] = 'x'
	r.left--
	return 1, nil
}

func TestStageSessionTranscriptCancellationRemovesPrivateStage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	reader := &pacedReader{started: make(chan struct{}), left: 100}
	done := make(chan error, 1)
	go func() {
		_, err := StageSessionTranscript(ctx, "subject", "runtime", "Stop", reader)
		done <- err
	}()
	<-reader.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stage cancellation = %v", err)
	}
	if _, _, ok := SessionCapturedTranscript("subject", "runtime"); ok {
		t.Fatal("cancelled stage became restorable")
	}
	temps, err := filepath.Glob(filepath.Join(TranscriptDir(), managedRecordsDir, "*", ".capture-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("cancelled staging files remain: %v", temps)
	}
}
