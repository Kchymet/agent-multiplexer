package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The session journal is amux's own record of what it did to a session, in the
// same append-only JSONL shape as the permission journal (permissions.go) and
// read by the same tailer — so a line written here reaches a subscribed
// orchestrator as a `notice` runtime event alongside the runtime's own events
// (docs/remote-provider-sessions.md §4.6).
//
// It exists because some things that happen to a session are amux's doing, not
// the runtime's, and the runtime therefore never writes them down. The case that
// forced it: a relayed `prompt` to a *stopped* agent is acknowledged immediately
// and the cold start happens afterwards, so "starting agent" — and a start that
// failed — have no other way to reach the caller (AGE-194).
//
// Unlike the permission journal, this one is keyed by the **amux session id**,
// not the runtime's conversation id: the daemon writes the first line before the
// runtime exists, so a conversation id is exactly what it does not yet have.

// Journal levels. They map onto the `notice` event's `level` field, so `error`
// here is the error a consumer sees on the stream.
const (
	JournalInfo  = "info"
	JournalError = "error"
)

// JournalRecord is one line of a session's amux journal.
type JournalRecord struct {
	Level string `json:"level"`
	Text  string `json:"text"`
	At    int64  `json:"at,omitempty"` // unix millis
}

// JournalDir holds the per-session amux journals.
func JournalDir() string { return filepath.Join(StateDir(), "journal") }

// JournalPath is the journal for one amux session id. An empty id yields "" —
// a caller with no session writes nothing.
func JournalPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return filepath.Join(JournalDir(), sanitizeID(sessionID)+".jsonl")
}

// AppendJournal appends one line to the session's journal, stamping At when the
// caller left it zero. Like the permission journal the write is a single
// O_APPEND of one short line, so concurrent writers interleave whole lines
// rather than tearing one. A blank id or text is a no-op.
func AppendJournal(sessionID, level, text string) error {
	path := JournalPath(sessionID)
	if path == "" || text == "" {
		return nil
	}
	if level == "" {
		level = JournalInfo
	}
	b, err := json.Marshal(JournalRecord{Level: level, Text: text, At: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(JournalDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadJournal returns every record in the session's journal, in order. A missing
// journal is empty, not an error — most sessions never have a line written.
// Unparsable lines are skipped here; the event reader keeps them as `raw`.
func ReadJournal(sessionID string) []JournalRecord {
	path := JournalPath(sessionID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []JournalRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r JournalRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Text == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}
