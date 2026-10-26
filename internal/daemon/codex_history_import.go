package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/runtimeevents"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// importCodexHistory is the one-time forward migration of a Codex session's
// PTY-era history into its structured event log. Central Codex auth runs every
// Codex session under the App Server, whose normalized log is the single
// canonical runtime-event source; the PTY rollout stays on disk (the App
// Server resumes and appends to that same rollout) but amux readers no longer
// look at it. Before the session's first structured log line, the rollout of
// the thread the launch will resume is mapped with the cold PTY reader and
// written as the log's ordered prefix.
//
// Idempotent: an existing log is canonical and is never rewritten, so a
// second run, or a session already structured, imports nothing. The log is
// published with a no-clobber link, so a concurrent first append wins rather
// than being duplicated or overwritten. The rollout is only read.
func importCodexHistory(s store.Session) (bool, error) {
	if agent.Canonical(s.Agent) != harnessproto.RuntimeCodex {
		return false, nil
	}
	path := codexapp.EventLogPathFor(s.ID)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	thread := s.ClaudeID
	if id, ok := codexapp.LoadIdentity(s.ID); ok && id.ThreadID != "" {
		thread = id.ThreadID
	}
	if thread == "" {
		return false, nil
	}
	pinned := s
	pinned.ClaudeID = thread
	rollout, ok := agent.HarnessFor(s.Agent).RuntimeTranscriptPath(pinned)
	if !ok {
		return false, nil
	}
	in, err := os.Open(rollout)
	if err != nil {
		return false, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".import-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriter(tmp)
	write := func(ev harnessproto.RuntimeEvent) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = w.Write(append(b, '\n'))
		return err
	}
	var state runtimeevents.CodexState
	lines := 0
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), 64<<20)
	for scanner.Scan() {
		lines++
		for _, ev := range runtimeevents.MapCodexLine(scanner.Bytes(), &state) {
			if err := write(ev); err != nil {
				_ = tmp.Close()
				return false, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	marker, _ := json.Marshal(map[string]any{"level": "info",
		"text": fmt.Sprintf("amux: imported %d lines of this Codex thread's earlier history; later events follow", lines)})
	if err := write(harnessproto.RuntimeEvent{Type: harnessproto.TypeNotice, Direction: harnessproto.DirMeta, Payload: marker}); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil // a first append won; that log is canonical
		}
		return false, err
	}
	return true, nil
}

// importCodexHistories runs the forward history migration for every stored
// Codex session, archived ones included. Failures are logged and retried on
// the next startup or launch; they never block the daemon.
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
