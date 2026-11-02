// Package directsvc is the daemon-managed half of `amux direct`: the persisted
// setting that opts this host into a direct listener, and the supervisor the
// daemon runs to keep `amux direct serve --managed` alive while that setting is
// on. See docs/direct-access.md ("Run it with the daemon").
//
// The listener is off by default. Only an explicit `amux direct enable` writes
// enabled=true; the daemon then starts the listener at its own startup and stops
// it when the setting is turned off or the daemon stops.
//
// This package deliberately does not import internal/direct (which imports the
// daemon). Full validation of a configuration — bind policy, canonical origins,
// a loadable certificate — happens in the CLI before it is saved and again in
// the child before it binds; the supervisor only needs the on/off switch and a
// fingerprint that tells it when to restart the child.
package directsvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"amux/internal/amuxcfg"
	"amux/internal/core"
)

// Section is the config.json section the setting lives in.
const Section = "direct"

// EnabledKey is the setting's name as `amux config ls` prints it.
const EnabledKey = "direct.enabled"

// Settings is the persisted direct-listener configuration. It holds no secret:
// the TLS key is referenced by path and stays wherever the operator keeps it,
// and the server identity and grants stay in the protected direct store. The
// JSON keys mirror the `amux direct serve` flags.
type Settings struct {
	Enabled         bool     `json:"enabled"`
	Listen          string   `json:"listen,omitempty"`
	Endpoints       []string `json:"endpoints,omitempty"`
	AllowOrigins    []string `json:"allow_origins,omitempty"`
	TLSCert         string   `json:"tls_cert,omitempty"`
	TLSKey          string   `json:"tls_key,omitempty"`
	AllowPublicBind bool     `json:"allow_public_bind,omitempty"`
	AccessTTL       string   `json:"access_ttl,omitempty"`
	IdleTTL         string   `json:"idle_ttl,omitempty"`
	AbsoluteTTL     string   `json:"absolute_ttl,omitempty"`
}

// Load reads the saved settings. set is false when the section is absent (the
// default: disabled). An unknown key is an error rather than a setting silently
// dropped, because a misspelt key here could change what is exposed.
func Load() (s Settings, set bool, err error) {
	top, err := amuxcfg.Read()
	if err != nil {
		return Settings{}, false, err
	}
	raw, ok := top[Section]
	if !ok {
		return Settings{}, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Settings{}, true, fmt.Errorf("%s: %s: %w", core.ConfigPath(), Section, err)
	}
	return s, true, nil
}

// Save writes the settings, preserving every other config section.
func Save(s Settings) error {
	top, err := amuxcfg.Read()
	if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	top[Section] = b
	return amuxcfg.Write(top)
}

// Args renders the settings as `amux direct serve` flags. It is what the
// managed child would have been run with by hand, for status and docs; the
// child itself reads the settings file, so nothing here reaches a process
// table. (There is no secret in it either way.)
func (s Settings) Args() []string {
	var a []string
	add := func(flag, v string) {
		if v != "" {
			a = append(a, "--"+flag, v)
		}
	}
	add("listen", s.Listen)
	for _, e := range s.Endpoints {
		add("endpoint", e)
	}
	for _, o := range s.AllowOrigins {
		add("allow-origin", o)
	}
	add("tls-cert", s.TLSCert)
	add("tls-key", s.TLSKey)
	if s.AllowPublicBind {
		a = append(a, "--allow-public-bind")
	}
	add("access-ttl", s.AccessTTL)
	add("idle-ttl", s.IdleTTL)
	add("absolute-ttl", s.AbsoluteTTL)
	return a
}

// Fingerprint identifies the configuration a child was started with. It covers
// the settings and the size and modification time of the certificate and key
// files, so renewing a certificate in place restarts the listener onto the new
// one, the same way restarting a manual `amux direct serve` would.
func Fingerprint(s Settings) string {
	h := sha256.New()
	b, _ := json.Marshal(s)
	h.Write(b)
	for _, p := range []string{s.TLSCert, s.TLSKey} {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "\x00%s\x00%d\x00%d", p, st.Size(), st.ModTime().UnixNano())
		} else {
			fmt.Fprintf(h, "\x00%s\x00missing", p)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
