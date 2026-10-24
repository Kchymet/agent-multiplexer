package amuxcfg

import (
	"encoding/json"
	"fmt"

	"amux/internal/core"
)

// Harness authentication modes. Central is amux's normal path: the host
// acquires one credential and the daemon delivers only a session credential to
// each authorized pane. The other modes are explicit compatibility selections.
const (
	// AuthCentral delivers the daemon's central credential to each session.
	AuthCentral = "central"
	// AuthHost inherits the host harness login through the legacy shared
	// credential file (Linux) or the macOS Keychain broker.
	AuthHost = "host"
	// AuthShared uses the legacy store created by older `amux auth login`.
	AuthShared = "shared"
	// AuthEnvironment provides no amux credential: API keys, apiKeyHelper,
	// cloud providers and gateways come from the inherited environment and the
	// harness's own settings.
	AuthEnvironment = "environment"
)

// AuthModes lists each harness's accepted values, default first.
func AuthModes(harness string) []string {
	switch harness {
	case "claude":
		return []string{AuthCentral, AuthHost, AuthShared, AuthEnvironment}
	case "codex":
		return []string{AuthCentral, AuthHost, AuthEnvironment}
	}
	return nil
}

// DefaultAuthMode is the effective mode when nothing is saved: central for
// every harness.
func DefaultAuthMode(harness string) string {
	if modes := AuthModes(harness); len(modes) > 0 {
		return modes[0]
	}
	return ""
}

func validAuthMode(harness, value string) bool {
	for _, mode := range AuthModes(harness) {
		if value == mode {
			return true
		}
	}
	return false
}

func authModeError(harness string) error {
	return fmt.Errorf("%s.auth must be one of %q", harness, AuthModes(harness))
}

func section(top map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	if b, ok := top[name]; ok {
		return object(b, core.ConfigPath()+": "+name)
	}
	return map[string]json.RawMessage{}, nil
}

// SavedAuthMode returns the persisted mode, or empty when unconfigured.
func SavedAuthMode(harness string) (string, error) {
	if AuthModes(harness) == nil {
		return "", fmt.Errorf("unknown harness %q", harness)
	}
	top, err := Read()
	if err != nil {
		return "", err
	}
	sec, err := section(top, harness)
	if err != nil {
		return "", err
	}
	b, ok := sec["auth"]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(b, &value); err != nil || !validAuthMode(harness, value) {
		return "", fmt.Errorf("%s: %w", core.ConfigPath(), authModeError(harness))
	}
	return value, nil
}

// AuthMode returns the effective mode. An invalid saved value is an error, so a
// typo can never silently select a different credential source.
func AuthMode(harness string) (string, error) {
	value, err := SavedAuthMode(harness)
	if err != nil || value != "" {
		return value, err
	}
	return DefaultAuthMode(harness), nil
}

// SetAuthMode saves a mode, or unsets it when value is empty.
func SetAuthMode(harness, value string) error {
	if AuthModes(harness) == nil {
		return fmt.Errorf("unknown harness %q", harness)
	}
	if value != "" && !validAuthMode(harness, value) {
		return authModeError(harness)
	}
	top, err := Read()
	if err != nil {
		return err
	}
	sec, err := section(top, harness)
	if err != nil {
		return err
	}
	if value == "" {
		delete(sec, "auth")
	} else {
		sec["auth"], _ = json.Marshal(value)
	}
	if len(sec) == 0 {
		delete(top, harness)
	} else {
		top[harness], _ = json.Marshal(sec)
	}
	return Write(top)
}
