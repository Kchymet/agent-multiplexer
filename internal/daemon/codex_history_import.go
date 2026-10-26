package daemon

import (
	"bufio"
	"bytes"
	"context"
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

// importMarkerKey tags the notice that ends an imported history prefix. The
// log itself thus records a published import, so a retry after a failed
// sidecar write recognizes it instead of importing the rollout twice.
const importMarkerKey = "amux_history_import"

// importCodexHistory is the one-time forward migration of a Codex session's
// PTY-era history into its structured event log. Central Codex auth runs every
// Codex session under the App Server, whose normalized log is the single
// canonical runtime-event source; the PTY rollout stays on disk (the App
// Server resumes and appends to that same rollout) but amux readers no longer
// look at it.
//
// The decision follows the log's actual content. A log with structured events
// (anything but amux notices) is already the session's history: its rollout
// then also holds the log's own turns, and log events carry no time to
// separate them, so it is left as is. Otherwise the rollout of the thread the
// next launch resumes (the persisted identity's, else the PTY-pinned id) is
// mapped with the cold PTY reader into the log's ordered prefix, ending in a
// versioned marker notice; notices already in the log follow it.
//
// The caller must hold the log quiescent (codexapp.Manager.WithQuiescentLog):
// no runtime is running or starting and AppendNotice is excluded. The outcome
// is recorded durably beside the log; until then a failure leaves the
// migration pending. The rollout is only read.
func importCodexHistory(s store.Session) (bool, error) {
	if agent.Canonical(s.Agent) != harnessproto.RuntimeCodex {
		return false, nil
	}
	if _, err := os.Stat(historyMarkerPath(s.ID)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err // decided (or unreadable state: retry later)
	}
	path := codexapp.EventLogPathFor(s.ID)
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	imported, structured := scanEventLog(existing)
	thread := s.ClaudeID
	if id, ok := codexapp.LoadIdentity(s.ID); ok && id.ThreadID != "" {
		thread = id.ThreadID
	}
	switch {
	case imported != nil:
		return false, writeHistoryMarker(s.ID, *imported) // published earlier
	case structured:
		return false, writeHistoryMarker(s.ID, historyMarker{Version: 1, Outcome: "structured", Thread: thread})
	case thread == "":
		return false, nil
	}
	pinned := s
	pinned.ClaudeID = thread
	rollout, ok := agent.HarnessFor(s.Agent).RuntimeTranscriptPath(pinned)
	if !ok {
		return false, nil // nothing (yet) to import
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".import-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	fail := func(err error) (bool, error) { _ = tmp.Close(); return false, err }
	m := historyMarker{Version: 1, Outcome: "imported", Thread: thread}
	if m.Lines, err = writeImportedRollout(tmp, rollout, m); err != nil {
		return fail(err)
	}
	// Notices already in the log (about this launch) follow the prefix.
	if _, err := tmp.Write(existing); err != nil {
		return fail(err)
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
	return true, writeHistoryMarker(s.ID, m)
}

// scanEventLog reports a published import marker and whether the log holds
// structured history (any event other than an amux notice).
func scanEventLog(log []byte) (*historyMarker, bool) {
	var marker *historyMarker
	structured := false
	for _, line := range bytes.Split(log, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev harnessproto.RuntimeEvent
		if json.Unmarshal(line, &ev) != nil || ev.Type != harnessproto.TypeNotice {
			structured = true
			continue
		}
		var p map[string]json.RawMessage
		if json.Unmarshal(ev.Payload, &p) == nil && len(p[importMarkerKey]) > 0 {
			var m historyMarker
			if json.Unmarshal(p[importMarkerKey], &m) == nil && m.Version == 1 && m.Outcome == "imported" {
				marker = &m
			}
		}
	}
	return marker, structured
}

// writeImportedRollout maps a rollout into normalized event lines followed by
// the versioned marker notice, and returns the number of rollout lines read.
func writeImportedRollout(w io.Writer, rollout string, m historyMarker) (int, error) {
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
	m.Lines = lines
	p, _ := json.Marshal(map[string]any{"level": "info", importMarkerKey: m,
		"text": fmt.Sprintf("amux: imported %d lines of this Codex thread's earlier history; later events follow", lines)})
	if err := write(harnessproto.RuntimeEvent{Type: harnessproto.TypeNotice, Direction: harnessproto.DirMeta, Payload: p}); err != nil {
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
		if err := d.importHistory(context.Background(), s); err != nil {
			log.Printf("amux: Codex history import for %s: %v", s.ID, err)
		}
	}
}

// importHistory runs the migration with the session's log held quiescent. A
// running or starting runtime is never rewritten under (ErrLogBusy): its own
// launch already ran the migration first.
func (d *Daemon) importHistory(ctx context.Context, s store.Session) error {
	if agent.Canonical(s.Agent) != harnessproto.RuntimeCodex {
		return nil
	}
	run := func() error { _, err := importCodexHistory(s); return err }
	if d.codex == nil {
		unlock := codexapp.LockEventLog(s.ID)
		defer unlock()
		return run()
	}
	if err := d.codex.WithQuiescentLog(ctx, s.ID, run); err != nil && !errors.Is(err, codexapp.ErrLogBusy) {
		return err
	}
	return nil
}
