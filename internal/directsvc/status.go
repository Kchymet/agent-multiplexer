package directsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"amux/internal/core"
)

// Supervisor states, as written to the status file.
const (
	// StateDisabled: the setting is off (or absent); no listener runs.
	StateDisabled = "disabled"
	// StateRunning: the managed child is alive.
	StateRunning = "running"
	// StateBackoff: the child exited or could not start; it is retried at NextRetry.
	StateBackoff = "backoff"
	// StateInvalid: the saved setting cannot be read; nothing runs until it is fixed.
	StateInvalid = "invalid"
	// StateStopped: the supervising daemon has stopped, so no listener runs.
	StateStopped = "stopped"
)

// Status is what the supervisor publishes for `amux direct status` and doctor.
// It carries no secret: the listen address and endpoints are already in
// config.json, and nothing else here identifies a credential.
type Status struct {
	State       string    `json:"state"`
	Enabled     bool      `json:"enabled"`
	Fingerprint string    `json:"fingerprint,omitempty"` // configuration the current child runs (or would run)
	PID         int       `json:"pid,omitempty"`         // managed child
	DaemonPID   int       `json:"daemon_pid"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	NextRetry   time.Time `json:"next_retry,omitempty"`
	Restarts    int       `json:"restarts"`
	LastError   string    `json:"last_error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StatusPath is the supervisor's status file.
func StatusPath() string { return filepath.Join(core.StateDir(), "direct-managed.json") }

// LogPath is where the managed child's stdout and stderr go.
func LogPath() string { return filepath.Join(core.StateDir(), "direct.log") }

// ReadStatus reads a status file.
func ReadStatus(path string) (Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// writeStatus replaces the status file atomically, so a reader never sees a
// torn record.
func writeStatus(path string, st Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".direct-managed-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Ready is written by the managed child once its listener and admin socket
// are up. It is what ties "a direct server answers" to "the daemon's child is
// that server": a manual `amux direct serve` can answer the same admin socket,
// and readiness must not be borrowed from it.
type Ready struct {
	PID         int       `json:"pid"`
	ServerID    string    `json:"server_id"`
	Addr        string    `json:"addr"`
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
}

// ReadyPath is the managed child's readiness marker.
func ReadyPath() string { return filepath.Join(core.StateDir(), "direct-managed-ready.json") }

// WriteReady records the managed child's readiness.
func WriteReady(path string, r Ready) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// ReadReady reads a readiness marker.
func ReadReady(path string) (Ready, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Ready{}, err
	}
	var r Ready
	err = json.Unmarshal(b, &r)
	return r, err
}

// Serving reports whether the readiness marker belongs to the child the
// supervisor is running now, with the configuration it was started for.
func (st Status) Serving(r *Ready) bool {
	return r != nil && st.State == StateRunning && st.PID != 0 && r.PID == st.PID && r.Fingerprint == st.Fingerprint
}
