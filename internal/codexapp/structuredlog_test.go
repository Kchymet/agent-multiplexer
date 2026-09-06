package codexapp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The supervisor's event log is APPEND-only: a cold-start notice the daemon wrote
// before the supervisor started must survive the supervisor's first emit (not be
// truncated away), so the single canonical source keeps one continuous ordered
// stream.
func TestWriteLogAppendsPreservingColdNotice(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "s.events.jsonl")
	cold, _ := json.Marshal(notice("info", "starting agent"))
	if err := os.WriteFile(logPath, append(cold, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Config{EventLogPath: logPath})
	s.emit(notice("info", "session established"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want cold notice preserved + emitted = 2 lines, got %d: %s", len(lines), data)
	}
	if !strings.Contains(lines[0], "starting agent") || !strings.Contains(lines[1], "session established") {
		t.Fatalf("append order/content wrong: %s", data)
	}
}

// AppendNotice writes a normalized notice event to the session's structured event
// log (the single canonical source), decodable by the same identity mapper the
// tailer uses — so a daemon-written cold-start/failure notice is indistinguishable
// on the stream from one the supervisor emitted.
func TestAppendNoticeWritesNormalizedNotice(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := AppendNotice("sess-x", "info", "starting agent"); err != nil {
		t.Fatal(err)
	}
	if err := AppendNotice("sess-x", "error", "boom"); err != nil {
		t.Fatal(err)
	}
	// A blank id or text is a no-op, never an error.
	if err := AppendNotice("", "info", "x"); err != nil {
		t.Fatalf("blank id: %v", err)
	}

	data, err := os.ReadFile(EventLogPathFor("sess-x"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 notice lines, got %d: %s", len(lines), data)
	}
	var ev harnessproto.RuntimeEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil || ev.Type != harnessproto.TypeNotice {
		t.Fatalf("line 0 is not a normalized notice event: %s (%v)", lines[0], err)
	}
	var p struct{ Level, Text string }
	_ = json.Unmarshal(ev.Payload, &p)
	if p.Level != "info" || p.Text != "starting agent" {
		t.Fatalf("notice payload = %+v", p)
	}
}
