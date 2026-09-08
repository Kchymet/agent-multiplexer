package core

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// This legacy journal format is preserved for existing trusted-host diagnostics
// and authoritative-history replay. Managed Claude hooks no longer write it:
// they are session-controlled observations and cannot prove that the runtime
// displayed a prompt. A managed runtime may expose a journal through
// RuntimePermissionPath only when a daemon-owned producer independently proves
// its occurrences (docs/remote-provider-sessions.md §4.5).
//
// The journal remains append-only JSONL, one file per legacy runtime session id.
// Existing records are not deleted or migrated. Generation-free history can be
// read but is deliberately nonanswerable under the live occurrence gate.

// Permission decisions a journal line can carry. Allow/Deny are the two a human
// (or the `permission` verb) can give; Cleared is amux's own honest answer for a
// request it knows is gone without knowing which way it went.
const (
	PermissionAllow   = "allow"
	PermissionDeny    = "deny"
	PermissionCleared = "cleared"
)

// PermissionRecord is one line of a session's permission journal. A line with an
// empty Decision opens a request; a line with one closes the request its
// RequestID names. Tool/Action/Options are carried on the opening line only.
type PermissionRecord struct {
	RequestID string   `json:"request_id"`
	Tool      string   `json:"tool,omitempty"`
	Action    string   `json:"action,omitempty"`
	Options   []string `json:"options,omitempty"`
	Decision  string   `json:"decision,omitempty"`
	At        int64    `json:"at,omitempty"` // unix millis
}

// Open reports whether this record opens a request rather than closing one.
func (r PermissionRecord) Open() bool { return r.Decision == "" }

// PermissionDir holds the per-session permission journals.
func PermissionDir() string { return filepath.Join(StateDir(), "permissions") }

// PermissionJournalPath is the journal for one runtime session id. Empty id
// yields "" — a caller with no identity writes nothing.
func PermissionJournalPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return filepath.Join(PermissionDir(), sanitizeID(sessionID)+".jsonl")
}

// NewPermissionID mints the id that names one permission prompt. It only has to
// be unique within a session's journal (that is the scope a `permission` verb
// resolves in), so a random token is both sufficient and stable — once written,
// the id for that prompt never changes.
func NewPermissionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing CSPRNG must not cost us the correlation entirely: fall back to
		// the clock, which is still unique within a session in practice.
		binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	}
	return "perm-" + hex.EncodeToString(b[:])
}

// AppendPermission appends one record to the session's journal, stamping At when
// the caller left it zero. The write is a single O_APPEND of one short line, so
// concurrent hooks interleave whole lines rather than tearing one. A blank
// session id is a no-op.
func AppendPermission(sessionID string, rec PermissionRecord) error {
	path := PermissionJournalPath(sessionID)
	if path == "" || rec.RequestID == "" {
		return nil
	}
	if rec.At == 0 {
		rec.At = time.Now().UnixMilli()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(PermissionDir(), 0o755); err != nil {
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

// ReadPermissions returns every record in the session's journal, in order. A
// missing journal is empty, not an error — a session that has never been
// prompted has nothing to read. Unparsable lines are skipped: a torn tail must
// not hide the records before it.
func ReadPermissions(sessionID string) []PermissionRecord {
	path := PermissionJournalPath(sessionID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []PermissionRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r PermissionRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.RequestID == "" {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PendingPermissions returns the requests still open in the session's journal —
// opened and never resolved — oldest first. Normally at most one; parallel tool
// calls can open several.
func PendingPermissions(sessionID string) []PermissionRecord {
	return pendingIn(ReadPermissions(sessionID))
}

// pendingIn folds a journal into the requests still open, preserving the order
// they were opened in. A resolution for an id we never saw opened is ignored.
func pendingIn(recs []PermissionRecord) []PermissionRecord {
	var open []PermissionRecord
	for _, r := range recs {
		if r.Open() {
			open = append(open, r)
			continue
		}
		for i, o := range open {
			if o.RequestID == r.RequestID {
				open = append(open[:i], open[i+1:]...)
				break
			}
		}
	}
	return open
}

// ResolvePermission closes an open request and returns the record it wrote.
// tool, when set, picks the open request for that tool — the hook that proves a
// prompt closed (PostToolUse, PermissionDenied) names its tool but not the
// request — falling back to the oldest open one. ok=false when nothing was open,
// which is the common case: those hooks fire on every tool call, prompted or not.
func ResolvePermission(sessionID, tool, decision string) (PermissionRecord, bool) {
	open := PendingPermissions(sessionID)
	if len(open) == 0 {
		return PermissionRecord{}, false
	}
	pick := open[0]
	for _, r := range open {
		if tool != "" && r.Tool == tool {
			pick = r
			break
		}
	}
	rec := PermissionRecord{RequestID: pick.RequestID, Tool: pick.Tool, Decision: decision}
	if err := AppendPermission(sessionID, rec); err != nil {
		return PermissionRecord{}, false
	}
	return rec, true
}

// ClearPermissions closes every request still open in the session's journal, for
// the hooks that prove no prompt can still be up (the turn stopped, the session
// ended). It records PermissionCleared rather than guessing a decision.
func ClearPermissions(sessionID string) int {
	open := PendingPermissions(sessionID)
	n := 0
	for _, r := range open {
		if AppendPermission(sessionID, PermissionRecord{
			RequestID: r.RequestID, Tool: r.Tool, Decision: PermissionCleared,
		}) == nil {
			n++
		}
	}
	return n
}
