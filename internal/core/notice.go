package core

import (
	"os"
	"path/filepath"
	"strings"
)

// Notices are short-lived, user-facing warnings amux attaches to a session and
// surfaces on the rail — e.g. a pinned Claude conversation that couldn't be
// resumed, so the fallback to a fresh session isn't silent. Like hook state
// they're keyed by the Claude session id and kept as a small file, so whichever
// process writes one (the CLI or the daemon's engine) the daemon's poll loop
// picks it up. They're advisory and best-effort; a missing notice just means no
// warning to show.

// NoticeDir holds the per-session rail warnings.
func NoticeDir() string { return filepath.Join(StateDir(), "notices") }

func noticePath(sessionID string) string {
	return filepath.Join(NoticeDir(), sanitizeID(sessionID))
}

// WriteNotice records a one-line warning for the Claude session id. A blank id
// or message is a no-op.
func WriteNotice(sessionID, msg string) error {
	msg = strings.TrimSpace(msg)
	if sessionID == "" || msg == "" {
		return nil
	}
	if err := os.MkdirAll(NoticeDir(), 0o755); err != nil {
		return err
	}
	dst := noticePath(sessionID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(msg), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Notice returns the current warning for the Claude session id, or "".
func Notice(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	b, err := os.ReadFile(noticePath(sessionID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearNotice removes any warning for the Claude session id (best-effort), so a
// stale message drops off the rail once the condition that raised it is gone.
func ClearNotice(sessionID string) {
	if sessionID == "" {
		return
	}
	_ = os.Remove(noticePath(sessionID))
}
