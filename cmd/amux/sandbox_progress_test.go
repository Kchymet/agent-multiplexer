package main

import (
	"bytes"
	"strings"
	"testing"
)

// drive feeds a progress callback what cfghome.ScanAll reports for n scans.
func drive(p func(done, total int), n int) {
	for i := 0; i <= n; i++ {
		p(i, n)
	}
}

// Off a terminal (a log, a wrapper capturing doctor) the hint is one plain
// line written as scanning starts, with no escape codes.
func TestScanProgressPlainWhenNotTerminal(t *testing.T) {
	var buf bytes.Buffer
	p := scanProgress(&buf, false, "comparing agent config copies")
	p(0, 80)
	if got := buf.String(); got != "  … comparing agent config copies (80)\n" {
		t.Fatalf("initial output = %q", got)
	}
	drive(p, 80)
	if out := buf.String(); strings.Count(out, "\n") != 2 || strings.Contains(out, "\033") || strings.Contains(out, "\r") {
		t.Fatalf("non-TTY output = %q, want only plain start lines", out)
	}
}

// On a terminal the line shows at once, counts up, and is erased at the end.
func TestScanProgressTerminalUpdatesAndClears(t *testing.T) {
	var buf bytes.Buffer
	p := scanProgress(&buf, true, "scanning")
	p(0, 3)
	if got := buf.String(); got != "\r\033[K  … scanning 0/3" {
		t.Fatalf("initial output = %q", got)
	}
	p(1, 3)
	p(2, 3)
	p(3, 3)
	if !strings.HasSuffix(buf.String(), "scanning 2/3\r\033[K") {
		t.Fatalf("output = %q, want the status line erased at the end", buf.String())
	}
}

// `amux sandbox drift --json` writes no progress at all, terminal or not.
func TestDriftProgressSilentForJSON(t *testing.T) {
	for _, tty := range []bool{false, true} {
		if p := driftProgress(true, &bytes.Buffer{}, tty); p != nil {
			t.Fatalf("tty=%v: --json got a progress callback", tty)
		}
		var buf bytes.Buffer
		drive(driftProgress(false, &buf, tty), 2)
		if buf.Len() == 0 {
			t.Fatalf("tty=%v: human drift output had no progress hint", tty)
		}
	}
}
