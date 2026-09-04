package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"amux/internal/core"
)

// The provider's connection states, as written to the status file. A machine
// registered as a compute node has no UI of its own — the status file is how
// `amux doctor` (and the user) sees whether the thing is actually connected,
// rather than guessing from a log tail.
const (
	StateDialing      = "dialing"      // trying to reach the orchestrator
	StateRegistered   = "registered"   // handshake accepted; serving panes
	StateDisconnected = "disconnected" // connection dropped; backing off to retry
	StateRejected     = "rejected"     // terminal registration failure; not retrying
	StateStopped      = "stopped"      // operator stop (ctx cancelled)
)

// Status is the provider loop's externally visible state. The moment fields are
// sticky: RegisteredAt and HeartbeatAt survive a disconnect so a report can say
// both "not connected now" and "last talked to the orchestrator 20s ago", which
// is the difference between a flap and a machine that never worked.
type Status struct {
	PID          int    `json:"pid"`
	Name         string `json:"name,omitempty"`
	Orchestrator string `json:"orchestrator,omitempty"`
	State        string `json:"state"`
	ProviderID   string `json:"provider_id,omitempty"`
	Panes        int    `json:"panes"`
	// The moments are written unconditionally: encoding/json's omitempty does not
	// fire on a zero time.Time, and a field that is always present is easier to
	// read than one that lies about being optional. A zero value reads as "never".
	UpdatedAt    time.Time `json:"updated_at"`
	RegisteredAt time.Time `json:"registered_at"`
	HeartbeatAt  time.Time `json:"heartbeat_at"`
	LastError    string    `json:"last_error,omitempty"`
}

// StatusPath is the file the provider loop writes its state to. It lives with
// the daemon pidfile and log — runtime state, not configuration.
func StatusPath() string { return filepath.Join(core.StateDir(), "provider-status.json") }

// WriteStatus writes a status snapshot atomically (temp file + rename), so a
// reader never sees a half-written record and a crash mid-write leaves the
// previous snapshot intact.
func WriteStatus(path string, s Status) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadStatus reads the status file. A missing file surfaces as an
// fs.ErrNotExist-wrapping error: "the provider has never run" is a normal state.
func ReadStatus(path string) (Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, err
	}
	return s, nil
}

// setStatus mutates the reported status and hands a copy to Config.OnStatus. It
// takes its own mutex rather than p.mu: status is reported from the dial loop,
// the read loop, and pane bookkeeping, and none of those should contend with (or
// deadlock against) the pane lock.
func (p *Provider) setStatus(mutate func(*Status)) {
	if p.cfg.OnStatus == nil {
		return
	}
	p.stMu.Lock()
	mutate(&p.status)
	p.status.UpdatedAt = time.Now()
	snapshot := p.status
	p.stMu.Unlock()
	p.cfg.OnStatus(snapshot)
}

// paneCount is the number of panes the provider currently owns, for the status
// report. Taken without the status lock held.
func (p *Provider) paneCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.panes)
}
