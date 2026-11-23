package direct

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
)

// The direct audit log: one JSON line per queue answer, item triage and
// overseer steer a direct client makes, in direct-audit.jsonl in the direct
// state directory (0600). A record is written before the daemon is asked to
// act (outcome "admitted") and again with the outcome; if the first write
// fails the request is refused and nothing runs. `amux device audit` reads it
// (host-only, over the admin socket or offline under host proof).

// AuditFileName is the log's file name in the direct state directory.
const AuditFileName = "direct-audit.jsonl"

// Audit bounds: the text kept per record and the size at which the log
// rotates to AuditFileName+".1" (one generation kept).
const (
	MaxAuditTextBytes = 4096
	MaxAuditFileBytes = 10 << 20
	// MaxAuditRead bounds the records one audit read returns (newest).
	MaxAuditRead = 500
)

// AuditRecord is one audit line. At is Unix milliseconds.
type AuditRecord struct {
	At       int64  `json:"at"`
	ClientID string `json:"client_id"`
	Label    string `json:"label,omitempty"`
	Route    string `json:"route"`          // queue.answer | items.state | overseer.steer
	Verb     string `json:"verb"`           // entry kind, triage state, or steer verb
	Target   string `json:"target"`         // entry, item or session id
	Text     string `json:"text,omitempty"` // the answer or prompt text, capped
	Outcome  string `json:"outcome"`        // admitted | ok | an error code
	Note     string `json:"note,omitempty"`
}

type auditRecord = AuditRecord

func (r AuditRecord) with(outcome string) AuditRecord {
	r.Outcome = outcome
	return r
}

type auditLog struct {
	mu   sync.Mutex
	path string
}

func capText(s string) string {
	if len(s) <= MaxAuditTextBytes {
		return s
	}
	s = s[:MaxAuditTextBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func (l *auditLog) append(r AuditRecord) error {
	if l == nil || l.path == "" {
		return errors.New("audit log unavailable")
	}
	r.Text = capText(r.Text)
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size()+int64(len(line)) > MaxAuditFileBytes {
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// read returns the newest records (oldest first), optionally for one client.
func (l *auditLog) read(clientID string, max int) ([]AuditRecord, error) {
	if max <= 0 || max > MaxAuditRead {
		max = MaxAuditRead
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []AuditRecord
	for _, p := range []string{l.path + ".1", l.path} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var r AuditRecord
			if json.Unmarshal(sc.Bytes(), &r) != nil {
				continue
			}
			if clientID != "" && r.ClientID != clientID {
				continue
			}
			out = append(out, r)
			if len(out) > max {
				out = out[1:]
			}
		}
		err = sc.Err()
		_ = f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Audit appends one record, stamped now.
func (a *Authority) Audit(r AuditRecord) error {
	r.At = a.now().UnixMilli()
	return a.audit.append(r)
}

// AuditRecords reads the newest audit records (oldest first).
func (a *Authority) AuditRecords(clientID string, max int) ([]AuditRecord, error) {
	return a.audit.read(clientID, max)
}

// ClientLabel is a grant's enrollment label ("" when unknown).
func (a *Authority) ClientLabel(clientID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if g, ok := a.state.Grants[clientID]; ok {
		return strings.TrimSpace(g.Label)
	}
	return ""
}

func newAuditLog(root string) *auditLog {
	return &auditLog{path: filepath.Join(root, AuditFileName)}
}
