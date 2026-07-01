package core

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
)

// TranscriptDir holds independent copies of agent conversation transcripts,
// captured by Claude Code hooks (see `amux agent capture`). It exists to debug
// the "restarting" bug: an agent's Claude transcript sometimes goes missing after
// a restart, so we snapshot Claude's own transcript at every hook event into a
// durable location and compare it against what Claude Code persisted itself.
func TranscriptDir() string { return filepath.Join(StateDir(), "transcripts") }

// CaptureTranscript copies Claude's live transcript (src, taken from a hook
// payload's transcript_path) to a stable per-session file, and appends a line to
// a per-session timeline log recording the event and whether the transcript was
// present. Best-effort and non-disruptive: a blank id/src is a no-op, and a
// missing src is logged (its absence at an event is itself a signal) but not an
// error the caller should act on. event and amuxID are recorded for
// cross-reference and may be empty.
func CaptureTranscript(sessionID, src, event, amuxID string) error {
	if sessionID == "" || src == "" {
		return nil
	}
	dir := TranscriptDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	size := int64(-1)
	if info, err := os.Stat(src); err == nil && !info.IsDir() {
		size = info.Size()
	}
	appendCaptureLog(dir, sessionID, event, amuxID, src, size)
	if size < 0 {
		return nil // transcript not on disk at this event; the log line records it
	}
	return copyFileAtomic(src, filepath.Join(dir, sanitizeID(sessionID)+".jsonl"))
}

// CapturedTranscript returns the path to amux's captured backup of sessionID's
// transcript (the file CaptureTranscript writes), its size, and whether it
// currently exists on disk. A blank sessionID yields ok=false. Callers use it to
// gap-fill a harness's own transcript that went missing across a restart.
func CapturedTranscript(sessionID string) (path string, size int64, ok bool) {
	if sessionID == "" {
		return "", 0, false
	}
	path = filepath.Join(TranscriptDir(), sanitizeID(sessionID)+".jsonl")
	if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
		return path, fi.Size(), true
	}
	return path, 0, false
}

// RestoreCapturedTranscript copies amux's captured backup of sessionID's
// transcript to dst (atomically, creating dst's directory), so a harness whose
// own transcript went missing across a restart resumes the real conversation
// instead of starting fresh. It restores only when the backup would not lose
// data: when dst is absent, when the backup is strictly larger (transcripts are
// append-only, so larger means more complete), or when they are the same size
// but the backup is newer. It never clobbers a dst that is larger or newer at
// equal size, so a fresher harness transcript is always preserved. Returns
// whether it wrote dst. A missing backup or blank sessionID is a no-op (false).
func RestoreCapturedTranscript(sessionID, dst string) (bool, error) {
	src, srcSize, ok := CapturedTranscript(sessionID)
	if !ok || dst == "" {
		return false, nil
	}
	if !shouldRestore(src, srcSize, dst) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	if err := copyFileAtomic(src, dst); err != nil {
		return false, err
	}
	return true, nil
}

// shouldRestore decides whether the backup at src (of size srcSize) should
// overwrite dst without losing data. See RestoreCapturedTranscript for the rule.
func shouldRestore(src string, srcSize int64, dst string) bool {
	di, err := os.Stat(dst)
	if err != nil || di.IsDir() {
		return true // dst absent (or a stray dir): nothing to lose
	}
	if srcSize > di.Size() {
		return true // backup is strictly more complete
	}
	if srcSize < di.Size() {
		return false // dst has more: never shrink it
	}
	// Same size: restore only if the backup is newer (a harmless refresh of a
	// near-identical file), never if dst is at least as fresh.
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	return si.ModTime().After(di.ModTime())
}

// captureLogLine is one entry in a session's capture timeline.
type captureLogLine struct {
	Ts      int64  `json:"ts"`
	Event   string `json:"event,omitempty"`
	AmuxID  string `json:"amux_id,omitempty"`
	Src     string `json:"transcript_path,omitempty"`
	Bytes   int64  `json:"bytes"`
	Present bool   `json:"present"`
}

func appendCaptureLog(dir, sessionID, event, amuxID, src string, size int64) {
	f, err := os.OpenFile(filepath.Join(dir, sanitizeID(sessionID)+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(captureLogLine{
		Ts: time.Now().UnixMilli(), Event: event, AmuxID: amuxID,
		Src: src, Bytes: size, Present: size >= 0,
	})
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// copyFileAtomic writes src's contents to dst via a temp file + rename, so a
// reader never sees a half-written transcript.
func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
