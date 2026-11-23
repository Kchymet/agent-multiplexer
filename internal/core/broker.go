package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The broker is the amux daemon/host that owns a set of sessions. Several
// brokers (machines) can run amux against the same GitHub repositories, so the
// branch every member commits on names its broker: amux/<broker>/<root>-<agent>
// (see BranchFor). That keeps two brokers' branches from colliding and lets each
// broker's PR watcher tell its own PRs apart.
//
// The broker id is chosen once and then kept stable:
//
//  1. AMUX_BROKER_ID, when set, wins (sanitized, never persisted).
//  2. Otherwise the id persisted in BrokerIDPath() (<data dir>/broker-id).
//     Edit that file to rename the broker; existing sessions keep the branch
//     they were created with.
//  3. Otherwise a new id is derived from the short hostname, persisted, and
//     reused from then on, so a later hostname change (DHCP, renamed laptop)
//     does not change it. With no usable hostname a random id is generated.

// BrokerIDEnv overrides the persisted broker id for this process.
const BrokerIDEnv = "AMUX_BROKER_ID"

// maxBrokerIDLen keeps branch names readable; longer ids are truncated.
const maxBrokerIDLen = 32

// BrokerIDPath is where the broker id is persisted.
func BrokerIDPath() string { return filepath.Join(DataDir(), "broker-id") }

var (
	brokerOnce sync.Once
	brokerID   string
)

// BrokerID returns this process's broker id, resolving (and persisting) it on
// first use. It is always a valid, non-empty single git ref component.
func BrokerID() string {
	brokerOnce.Do(func() {
		if testing.Testing() && os.Getenv(BrokerIDEnv) == "" {
			// Test binaries never touch the user's data dir or depend on the
			// machine's hostname.
			brokerID = TestBrokerID
			return
		}
		brokerID = resolveBrokerID(BrokerIDPath())
	})
	return brokerID
}

// TestBrokerID is the broker id test binaries see unless AMUX_BROKER_ID is set.
const TestBrokerID = "testbroker"

// SetBrokerIDForTest pins the broker id and returns a restore func. Tests use it
// so branch names do not depend on the machine or on a persisted file.
func SetBrokerIDForTest(id string) (restore func()) {
	BrokerID() // settle the Once so it cannot overwrite the pinned value later
	prev := brokerID
	brokerID = SanitizeBrokerID(id)
	return func() { brokerID = prev }
}

func resolveBrokerID(path string) string {
	if v := SanitizeBrokerID(os.Getenv(BrokerIDEnv)); v != "" {
		return v
	}
	if b, err := os.ReadFile(path); err == nil {
		if v := SanitizeBrokerID(string(b)); v != "" {
			return v
		}
	}
	id := defaultBrokerID()
	if err := persistBrokerID(path, id); err != nil {
		fmt.Fprintf(os.Stderr, "amux: could not persist broker id %q to %s: %v\n", id, path, err)
	}
	return id
}

func defaultBrokerID() string {
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".") // short hostname: drop .local / domain
	if v := SanitizeBrokerID(host); v != "" && v != "localhost" {
		return v
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "broker-" + hex.EncodeToString(b[:])
}

func persistBrokerID(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".broker-id-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(id + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Link rather than rename so two processes racing on first use agree on
	// whichever id landed first instead of the last writer silently winning.
	if err := os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// SanitizeBrokerID maps s onto a single git ref component: lower-case
// [a-z0-9-], runs of anything else collapsed to one '-', no leading/trailing
// '-', at most maxBrokerIDLen bytes. It returns "" when nothing usable remains.
//
// The result never looks like a legacy branch name. Legacy branches are
// amux/<root> and amux/<root>-<agent>[-suffix] where <root> is a six-hex-digit
// session id, and git cannot hold both a ref amux/X and refs under amux/X/, so
// an id that starts with six hex digits followed by '-' or the end is prefixed
// with "b-". That makes amux/<broker>/… and every legacy ref disjoint.
func SanitizeBrokerID(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return ""
	}
	if looksLikeLegacyRoot(out) {
		out = "b-" + out
	}
	if len(out) > maxBrokerIDLen {
		out = strings.TrimRight(out[:maxBrokerIDLen], "-")
	}
	return out
}

func looksLikeLegacyRoot(s string) bool {
	if len(s) < 6 {
		return false
	}
	for _, r := range s[:6] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return len(s) == 6 || s[6] == '-'
}
