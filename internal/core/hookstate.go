package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code hooks bridge the agent's lifecycle into amux: the `amux hook`
// subcommand writes a session's current activity here on each hook event, and
// the daemon's poll loop reads it back to drive the rail status. Records are
// keyed by the Claude session id (the same uuid amux pins as Session.ClaudeID).
// Because the hooks are installed at the user level, every Claude session writes
// here — including ones amux didn't launch, which the rail surfaces as untracked.

// HookRecord is the last activity recorded for a Claude session.
type HookRecord struct {
	State   string `json:"state"`         // idle | ready | waiting | running
	Cwd     string `json:"cwd,omitempty"` // the session's working directory
	Updated int64  `json:"updated"`       // unix millis of the last hook event
}

// HookStateDir holds the per-session activity files written by Claude hooks.
func HookStateDir() string { return filepath.Join(StateDir(), "hooks") }

func hookStatePath(sessionID string) string {
	return filepath.Join(HookStateDir(), sanitizeID(sessionID))
}

// WriteHookState records state (and cwd) for the Claude session id, stamped now
// and written atomically so the daemon never reads a torn file. A blank id is a
// no-op.
func WriteHookState(sessionID, state, cwd string) error {
	if sessionID == "" {
		return nil
	}
	if err := os.MkdirAll(HookStateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(HookRecord{State: state, Cwd: cwd, Updated: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	dst := hookStatePath(sessionID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// HookState returns the last record for the Claude session id, and whether any
// was found.
func HookState(sessionID string) (HookRecord, bool) {
	if sessionID == "" {
		return HookRecord{}, false
	}
	return readHookRecord(hookStatePath(sessionID))
}

// AllHookStates returns every recorded session keyed by Claude session id.
func AllHookStates() map[string]HookRecord {
	out := map[string]HookRecord{}
	ents, err := os.ReadDir(HookStateDir())
	if err != nil {
		return out
	}
	for _, e := range ents {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if r, ok := readHookRecord(filepath.Join(HookStateDir(), e.Name())); ok {
			out[e.Name()] = r
		}
	}
	return out
}

func readHookRecord(path string) (HookRecord, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return HookRecord{}, false
	}
	var r HookRecord
	if err := json.Unmarshal(b, &r); err != nil {
		// Tolerate a bare state word (an older record format).
		s := strings.TrimSpace(string(b))
		if s == "" {
			return HookRecord{}, false
		}
		return HookRecord{State: s}, true
	}
	if r.State == "" {
		return HookRecord{}, false
	}
	return r, true
}

// sanitizeID keeps a session id safe as a single path component (session ids are
// uuids, so this is just defense against an unexpected value).
func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
}
