package vterm

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The emulator drops OSC 52, so before OnClipboard an agent's "copied" never
// left the pane. Writes must reach the callback however the stream is chunked
// and whichever terminator the agent uses; queries and other targets must not,
// and none of it may print into the screen.
func TestOSC52ReachesOnClipboard(t *testing.T) {
	const hi = "aGk=" // base64("hi")
	for _, tc := range []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"BEL", []string{"\x1b]52;c;" + hi + "\a"}, []string{"hi"}},
		{"ST", []string{"\x1b]52;c;" + hi + "\x1b\\"}, []string{"hi"}},
		{"split in the introducer", []string{"\x1b]5", "2;c;" + hi + "\a"}, []string{"hi"}},
		{"split in the payload and before ST", []string{"\x1b]52;c;aG", "k=\x1b", "\\"}, []string{"hi"}},
		{"bytewise", strings.Split("\x1b]52;c;"+hi+"\a", ""), []string{"hi"}},
		{"query is not answered", []string{"\x1b]52;c;?\a"}, nil},
		{"primary only", []string{"\x1b]52;p;" + hi + "\a"}, nil},
		{"two copies", []string{"\x1b]52;c;" + hi + "\a\x1b]52;c;eW8=\a"}, []string{"hi", "yo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(20, 3)
			defer term.Close()
			var got []string
			term.OnClipboard(func(b []byte) { got = append(got, string(b)) })
			for _, c := range tc.chunks {
				term.ingest([]byte(c))
			}
			term.ingest([]byte("ok"))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("clipboard writes = %q, want %q", got, tc.want)
			}
			if line := strings.TrimRight(strings.SplitN(term.Render(), "\n", 2)[0], " "); line != "ok" {
				t.Errorf("screen row 0 = %q: the sequence leaked into the screen", line)
			}
		})
	}
}

// End to end through a real PTY: a child printing OSC 52 with printf.
func TestOSC52FromChildProcess(t *testing.T) {
	term := New(40, 5)
	defer term.Close()
	var mu sync.Mutex
	var got string
	term.OnClipboard(func(b []byte) { mu.Lock(); got = string(b); mu.Unlock() })
	if err := term.Start(exec.Command("sh", "-c", `printf '\033]52;c;Y29waWVkIGZyb20gY2hpbGQ=\007'; sleep 1`)); err != nil {
		t.Fatal(err)
	}
	if !waitFor(3*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return got != "" }) {
		t.Fatal("OSC 52 from the child never reached OnClipboard")
	}
	mu.Lock()
	defer mu.Unlock()
	if got != "copied from child" {
		t.Fatalf("clipboard = %q", got)
	}
}
