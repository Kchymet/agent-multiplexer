package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/runtimeevents"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// maxImportLine bounds one rollout line read by the history import. A longer
// line is replaced by an omission notice rather than failing the migration.
var maxImportLine = 16 << 20

// historyMarker is the durable outcome of a session's history migration,
// stored beside its structured event log. Without it the migration is pending
// and retried by the next startup or launch.
type historyMarker struct {
	Version int    `json:"v"`
	Outcome string `json:"outcome"` // imported | structured
	Thread  string `json:"thread,omitempty"`
	Lines   int    `json:"lines,omitempty"`
}

func historyMarkerPath(id string) string { return codexapp.EventLogPathFor(id) + ".history.json" }

// importCodexHistory is the one-time forward migration of a Codex session's
// PTY-era history into its structured event log. Central Codex auth runs every
// Codex session under the App Server, whose normalized log is the single
// canonical runtime-event source; the PTY rollout stays on disk (the App
// Server resumes and appends to that same rollout) but amux readers no longer
// look at it.
//
// For a session that has never run structured (no persisted identity), the
// rollout of the thread the launch will resume is mapped with the cold PTY
// reader and becomes the log's ordered prefix. Lines already in the log can
// only be amux's own notices about this launch; they are preserved after the
// prefix, including any appended while the log is replaced. A session that
// already ran structured keeps its log as is: the rollout then also holds the
// log's own turns, and log events carry no time to separate them, so it is
// recorded as structured rather than risk duplicating history.
//
// The outcome is recorded durably, so the migration runs at most once; until
// then any failure leaves it pending and is retried. The rollout is only read.
func importCodexHistory(s store.Session) (bool, error) {
	if agent.Canonical(s.Agent) != harnessproto.RuntimeCodex {
		return false, nil
	}
	if _, err := os.Stat(historyMarkerPath(s.ID)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err // decided (or unreadable state: retry later)
	}
	if id, ok := codexapp.LoadIdentity(s.ID); ok && id.ThreadID != "" {
		return false, writeHistoryMarker(s.ID, historyMarker{Version: 1, Outcome: "structured", Thread: id.ThreadID})
	}
	if s.ClaudeID == "" {
		return false, nil
	}
	rollout, ok := agent.HarnessFor(s.Agent).RuntimeTranscriptPath(s)
	if !ok {
		return false, nil // nothing (yet) to import
	}
	path := codexapp.EventLogPathFor(s.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".import-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	fail := func(err error) (bool, error) { _ = tmp.Close(); return false, err }
	lines, err := writeImportedRollout(tmp, rollout)
	if err != nil {
		return fail(err)
	}
	// Keep any lines already in the log (notices about this launch) after the
	// imported prefix, and carry over appends that race the replacement.
	old, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	var copied int64
	if old != nil {
		defer old.Close()
		if copied, err = io.Copy(tmp, old); err != nil {
			return fail(err)
		}
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	if old != nil {
		if _, err := old.Seek(copied, io.SeekStart); err == nil {
			if late, _ := io.ReadAll(old); len(late) > 0 {
				if f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
					_, _ = f.Write(late)
					_ = f.Close()
				}
			}
		}
	}
	return true, writeHistoryMarker(s.ID, historyMarker{Version: 1, Outcome: "imported", Thread: s.ClaudeID, Lines: lines})
}

// writeImportedRollout maps a rollout into normalized event lines followed by
// a marker notice, and returns the number of rollout lines read.
func writeImportedRollout(w io.Writer, rollout string) (int, error) {
	in, err := os.Open(rollout)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out := bufio.NewWriter(w)
	write := func(ev harnessproto.RuntimeEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = out.Write(append(b, '\n'))
		return err
	}
	notice := func(text string) harnessproto.RuntimeEvent {
		p, _ := json.Marshal(map[string]any{"level": "info", "text": text})
		return harnessproto.RuntimeEvent{Type: harnessproto.TypeNotice, Direction: harnessproto.DirMeta, Payload: p}
	}
	var state runtimeevents.CodexState
	r := bufio.NewReaderSize(in, 64<<10)
	lines := 0
	for {
		// Accumulate one line up to the bound; an oversized line is drained
		// without being held and replaced by an omission notice.
		var buf []byte
		size := 0
		var err error
		for {
			var part []byte
			part, err = r.ReadSlice('\n')
			size += len(part)
			if size <= maxImportLine {
				buf = append(buf, part...)
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		if err != nil && err != io.EOF {
			return lines, err
		}
		if size > 0 {
			lines++
			if size > maxImportLine {
				if werr := write(notice(fmt.Sprintf("amux: omitted rollout line %d (%d bytes) from imported history", lines, size))); werr != nil {
					return lines, werr
				}
			} else {
				for _, ev := range runtimeevents.MapCodexLine(buf, &state) {
					if werr := write(ev); werr != nil {
						return lines, werr
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if err := write(notice(fmt.Sprintf("amux: imported %d lines of this Codex thread's earlier history; later events follow", lines))); err != nil {
		return lines, err
	}
	return lines, out.Flush()
}

func writeHistoryMarker(id string, m historyMarker) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	path := historyMarkerPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// importCodexHistories runs the forward history migration for every stored
// Codex session, archived ones included. A failure is logged and stays
// pending; the session's next launch retries it and refuses to start without
// it, so the daemon itself always starts.
func (d *Daemon) importCodexHistories() {
	db, err := store.Open()
	if err != nil {
		log.Printf("amux: Codex history import: %v", err)
		return
	}
	rows, err := db.AllSessions()
	db.Close()
	if err != nil {
		log.Printf("amux: Codex history import: %v", err)
		return
	}
	for _, s := range rows {
		if _, err := importCodexHistory(s); err != nil {
			log.Printf("amux: Codex history import for %s: %v", s.ID, err)
		}
	}
}
