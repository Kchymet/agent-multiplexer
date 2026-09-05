package codexapp

import (
	"encoding/json"
	"os"
	"path/filepath"

	"amux/internal/core"
)

// identity.go persists a structured session's server/thread identity so it
// survives a daemon restart: the reconnecting daemon reads it to re-supervise the
// same thread (thread/resume) on the same socket, and a native CLI attach uses the
// same pair. It is amux-internal state — deliberately NOT on the wire, because the
// socket lives inside the session's private sandbox scope (AGE-181 §2).
//
// It is a per-session JSON sidecar under amux's state dir, mirroring how amux
// already persists live-agents and per-session notices/journal — not a store
// column, so it carries no schema migration and is trivially removable when a
// session leaves structured mode.

// stateDir is where identity sidecars live: <state>/codexapp.
func stateDir() string { return filepath.Join(core.StateDir(), "codexapp") }

// identityPath is the sidecar path for a session id.
func identityPath(sessionID string) string {
	return filepath.Join(stateDir(), sanitize(sessionID)+".json")
}

// EventLogPathFor is the per-session NDJSON runtime-event record the supervisor
// appends to and the provider's tailer reads (docs/codex-app-server-supervision.md).
// It lives under amux's durable data dir (not the tmpfs runtime dir): it is a
// transcript-like record a reconnecting reader resumes from, not an ephemeral
// socket.
func EventLogPathFor(sessionID string) string {
	return filepath.Join(core.DataDir(), "codexapp", sanitize(sessionID)+".events.jsonl")
}

// SocketPathFor derives the per-session App Server Unix socket path. It lives in
// the per-user runtime dir (tmpfs where available) under a codexapp subdir, kept
// short so it stays within the OS's sun_path limit (~108 bytes). The name is
// derived from the session id, so the supervisor, a reconnecting daemon, and a
// native CLI attach all compute the same path.
func SocketPathFor(sessionID string) string {
	base := runtimeDir()
	return filepath.Join(base, "codexapp", sanitize(sessionID)+".sock")
}

// EndpointFor is the default App Server endpoint for a session: WebSocket over the
// per-session Unix socket (`unix://<path>`), which keeps the endpoint inside the
// session's private sandbox scope. A loopback/WSS endpoint can be supplied via
// Config.Endpoint instead when portability is needed.
func EndpointFor(sessionID string) string {
	return "unix://" + SocketPathFor(sessionID)
}

// runtimeDir prefers $XDG_RUNTIME_DIR (per-user tmpfs) for the socket, falling
// back to amux's state dir where it is unset (e.g. macOS). It mirrors core's own
// unexported runtimeDir so socket placement matches the daemon control socket.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return core.StateDir()
}

// SaveIdentity writes id atomically. Best-effort persistence: a failure is
// returned so a caller can log it, but it must never block supervision.
func SaveIdentity(id Identity) error {
	if id.SessionID == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(id)
	if err != nil {
		return err
	}
	path := identityPath(id.SessionID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadIdentity reads a session's persisted identity. ok=false when none exists
// (never launched in structured mode, or already cleared) — never an error for a
// missing file.
func LoadIdentity(sessionID string) (Identity, bool) {
	b, err := os.ReadFile(identityPath(sessionID))
	if err != nil {
		return Identity{}, false
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return Identity{}, false
	}
	return id, true
}

// RemoveIdentity deletes a session's persisted identity (it left structured mode
// or was archived/deleted). A missing file is not an error.
func RemoveIdentity(sessionID string) error {
	err := os.Remove(identityPath(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// sanitize keeps a session id safe as a path segment (ids are uuids/short ids in
// practice, but never trust them into a filename): keep [A-Za-z0-9._-], drop the
// rest. An empty result falls back to "session" so a path is always well-formed.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "session"
	}
	return string(out)
}
