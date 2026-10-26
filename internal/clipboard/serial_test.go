package clipboard

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

type memoryFile struct{ bytes.Buffer }

func (*memoryFile) Fd() uintptr  { return ^uintptr(0) }
func (*memoryFile) Close() error { return nil }

func TestClipboardRequestContainsOnlyExplicitSelection(t *testing.T) {
	f := &memoryFile{}
	o := &SerialFile{File: f}

	selected := "界e\u0301 👩‍💻\nsecond line\x1b]52;c;?\a"
	if _, err := o.Write(Sequence([]byte(selected), false)); err != nil {
		t.Fatal(err)
	}
	request := f.String()
	if !strings.HasPrefix(request, "\x1b]52;c;") || !strings.HasSuffix(request, "\a") || strings.Count(request, "\x1b") != 1 {
		t.Fatalf("expected one system clipboard write, got %q", request)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(request, "\x1b]52;c;"), "\a")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || string(decoded) != selected {
		t.Fatalf("decoded clipboard = %q, %v; want %q", decoded, err, selected)
	}
	// Copy text is encoded even if it contains terminal controls; none becomes
	// a second request or a clipboard query in the outer terminal byte stream.
	if strings.Contains(request, ";?") {
		t.Fatal("clipboard request queried existing clipboard contents")
	}
}

// fragmentFile deliberately yields between individual bytes. Its own lock only
// protects the test buffer, not whole requests, so concurrent Write calls would
// interleave terminal sequences and set overlap even under the race detector.
type fragmentFile struct {
	memoryFile
	mu      sync.Mutex
	active  atomic.Int32
	overlap atomic.Bool
}

func (f *fragmentFile) Write(p []byte) (int, error) {
	if f.active.Add(1) != 1 {
		f.overlap.Store(true)
	}
	defer f.active.Add(-1)
	for _, b := range p {
		f.mu.Lock()
		f.WriteByte(b)
		f.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func TestRenderAndClipboardRequestsCannotInterleave(t *testing.T) {
	f := &fragmentFile{}
	o := &SerialFile{File: f}
	const frame = "\x1b[1;1Hrendered pane\x1b[0m"
	const request = "\x1b]52;c;c2VsZWN0ZWQ=\a" // independently encoded "selected"
	const workers = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range workers {
		wg.Go(func() {
			<-start
			if _, err := io.WriteString(o, frame); err != nil {
				t.Error(err)
			}
			if _, err := o.Write(Sequence([]byte("selected"), false)); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if f.overlap.Load() {
		t.Fatal("render and clipboard writes overlapped")
	}
	stream := f.String()
	frames, copies := 0, 0
	for stream != "" {
		switch {
		case strings.HasPrefix(stream, frame):
			frames++
			stream = strings.TrimPrefix(stream, frame)
		case strings.HasPrefix(stream, request):
			copies++
			stream = strings.TrimPrefix(stream, request)
		default:
			t.Fatalf("interleaved or unexpected terminal bytes: %q", stream)
		}
	}
	if frames != workers || copies != workers {
		t.Fatalf("got %d frames and %d copies, want %d each", frames, copies, workers)
	}
}

type failingFile struct {
	memoryFile
	err error
}

func (f *failingFile) Write(p []byte) (int, error) { return len(p) / 2, f.err }

func TestClipboardReportsWriteFailures(t *testing.T) {
	want := errors.New("terminal disconnected")
	for _, err := range []error{nil, want} {
		o := &SerialFile{File: &failingFile{err: err}}
		_, got := o.Write(Sequence([]byte("selected"), false))
		if err == nil && !errors.Is(got, io.ErrShortWrite) || err != nil && !errors.Is(got, want) {
			t.Fatalf("underlying error %v: Copy error = %v", err, got)
		}
	}
}

func TestOutputRetainsTerminalDetectionAndResize(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	o := &SerialFile{File: slave}
	var terminal term.File = o // the exact interface Bubble Tea asserts
	if terminal.Fd() != slave.Fd() || !term.IsTerminal(terminal.Fd()) {
		t.Fatal("wrapper lost the terminal descriptor or TTY detection")
	}
	for _, size := range []pty.Winsize{{Rows: 24, Cols: 80}, {Rows: 37, Cols: 123}} {
		if err := pty.Setsize(master, &size); err != nil {
			t.Fatal(err)
		}
		w, h, err := term.GetSize(terminal.Fd())
		if err != nil || w != int(size.Cols) || h != int(size.Rows) {
			t.Fatalf("wrapped terminal size = %dx%d, %v; want %dx%d", w, h, err, size.Cols, size.Rows)
		}
	}
}
