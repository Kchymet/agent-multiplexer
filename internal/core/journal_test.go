package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJournalRoundTrip covers the shape the event reader depends on: lines in
// order, one JSON object per line, a stamped time, and a default level — plus
// the no-ops, since the daemon calls this on a path where a failure to record
// must never be worse than the thing it was recording.
func TestJournalRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := AppendJournal("a1", JournalInfo, "starting agent"); err != nil {
		t.Fatal(err)
	}
	if err := AppendJournal("a1", "", "no level given"); err != nil {
		t.Fatal(err)
	}
	if err := AppendJournal("a1", JournalError, "start agent a1: permission denied"); err != nil {
		t.Fatal(err)
	}
	// Neither a session without an id nor a line without text is worth a file.
	if err := AppendJournal("", JournalInfo, "nobody"); err != nil {
		t.Fatal(err)
	}
	if err := AppendJournal("a2", JournalInfo, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(JournalPath("a2")); !os.IsNotExist(err) {
		t.Errorf("an empty line created %s", JournalPath("a2"))
	}
	if JournalPath("") != "" {
		t.Errorf("JournalPath(\"\") = %q, want empty", JournalPath(""))
	}

	got := ReadJournal("a1")
	if len(got) != 3 {
		t.Fatalf("read %+v, want three records in order", got)
	}
	if got[0].Text != "starting agent" || got[0].Level != JournalInfo {
		t.Errorf("first = %+v, want the starting notice", got[0])
	}
	if got[1].Level != JournalInfo {
		t.Errorf("level-less record = %+v, want it defaulted to %q", got[1], JournalInfo)
	}
	if got[2].Level != JournalError {
		t.Errorf("last = %+v, want the error level", got[2])
	}
	for i, r := range got {
		if r.At == 0 {
			t.Errorf("record %d has no timestamp: %+v", i, r)
		}
	}

	// The tailer reads this file line by line, so every record must be exactly one
	// line — a torn or multi-line write would desynchronize the ordinals.
	b, err := os.ReadFile(JournalPath("a1"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(b), "\n"); lines != 3 {
		t.Errorf("journal has %d newlines for 3 records:\n%s", lines, b)
	}

	// A session that was never written to reads empty rather than failing.
	if got := ReadJournal("never"); got != nil {
		t.Errorf("ReadJournal on a missing journal = %+v, want nothing", got)
	}
}

// TestJournalIsKeyedByAmuxSessionID guards the one thing that separates this
// journal from the permission journal: it is named by the amux session id, not
// the runtime's conversation id — because the daemon writes its first line
// before the runtime, and therefore the conversation, exists.
func TestJournalIsKeyedByAmuxSessionID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got, want := JournalPath("a1"), filepath.Join(JournalDir(), "a1.jsonl"); got != want {
		t.Errorf("JournalPath(a1) = %q, want %q", got, want)
	}
	if JournalDir() == PermissionDir() {
		t.Error("the amux journal and the permission journal must not share a directory")
	}
}
