// Package amuxcfg reads amux's own config.json, shared by daemon rollout
// settings and TUI keybindings. It does not configure the agent CLIs.
package amuxcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"amux/internal/core"
)

// Read preserves sections owned by other settings (including unknown ones).
func Read() (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(core.ConfigPath())
	if os.IsNotExist(err) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	return object(b, core.ConfigPath())
}

func object(b []byte, name string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if m == nil {
		return nil, fmt.Errorf("%s: expected a JSON object", name)
	}
	return m, nil
}

// Write atomically replaces the document after the caller edits its section.
func Write(top map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(core.ConfigDir(), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(core.ConfigDir(), ".config-*")
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
	return os.Rename(f.Name(), core.ConfigPath())
}

const (
	AppServer  = "app-server"
	PTY        = "pty"
	ControlEnv = "AMUX_CODEX_CONTROL"
)

func codexSection(top map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if b, ok := top["codex"]; ok {
		return object(b, core.ConfigPath()+": codex")
	}
	return map[string]json.RawMessage{}, nil
}

// CodexControl returns the persisted value, or empty when unconfigured. It
// deliberately ignores the environment: config get describes the saved setting.
func CodexControl() (string, error) {
	top, err := Read()
	if err != nil {
		return "", err
	}
	codex, err := codexSection(top)
	if err != nil {
		return "", err
	}
	b, ok := codex["control"]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(b, &value); err != nil || (value != AppServer && value != PTY) {
		return "", fmt.Errorf("%s: codex.control must be %q or %q", core.ConfigPath(), AppServer, PTY)
	}
	return value, nil
}

// SetCodexControl saves a value, or unsets it when value is empty. Reading the
// raw section lets this repair an invalid control while preserving sibling keys.
func SetCodexControl(value string) error {
	if value != "" && value != AppServer && value != PTY {
		return fmt.Errorf("codex.control must be %q or %q", AppServer, PTY)
	}
	top, err := Read()
	if err != nil {
		return err
	}
	codex, err := codexSection(top)
	if err != nil {
		return err
	}
	if value == "" {
		delete(codex, "control")
	} else {
		codex["control"], _ = json.Marshal(value)
	}
	if len(codex) == 0 {
		delete(top, "codex")
	} else {
		top["codex"], _ = json.Marshal(codex)
	}
	return Write(top)
}

// Control is the immutable selection captured when a daemon is constructed.
// The daemon returns this over its local diagnostic query; it is never inferred
// from a client's environment or from files edited after startup.
type Control struct {
	ConfigPath  string `json:"config_path"`
	Persisted   string `json:"persisted"`
	Override    string `json:"override"`
	OverrideSet bool   `json:"override_set"`
	Effective   string `json:"effective"`
	Source      string `json:"source"`
	Warning     string `json:"warning,omitempty"`
}

// ResolveCodexControl validates disk even when an override is present, so a
// broken opt-in cannot silently fall back on the next restart. Empty (including
// whitespace) overrides defer to disk. Other non-app-server values retain the
// legacy PTY behavior; only "pty" is a recognized explicit disable.
func ResolveCodexControl() (Control, error) {
	c := Control{ConfigPath: core.ConfigPath(), Effective: PTY, Source: "default"}
	c.Override, c.OverrideSet = os.LookupEnv(ControlEnv)
	var err error
	c.Persisted, err = CodexControl()
	if err != nil {
		return c, err
	}
	if c.Persisted != "" {
		c.Effective, c.Source = c.Persisted, "config"
	}
	if value := strings.ToLower(strings.TrimSpace(c.Override)); value != "" {
		c.Effective, c.Source = PTY, "environment"
		switch value {
		case AppServer:
			c.Effective = AppServer
		case PTY:
		default:
			c.Warning = fmt.Sprintf("unrecognized %s=%q; using legacy pty override (use app-server or pty, or unset it)", ControlEnv, c.Override)
		}
	}
	// Central Codex auth delivers tokens through the supervised App Server's
	// external-auth flow; a PTY Codex has no equivalent input. Explicit host and
	// environment modes keep the selection above.
	mode, err := AuthMode("codex")
	if err != nil {
		return c, err
	}
	if mode == AuthCentral && c.Effective != AppServer {
		if c.Source != "default" {
			c.Warning = fmt.Sprintf("codex.auth is central, which requires App Server control; ignoring codex.control %s from %s (run `amux config set codex.auth host` to keep it)", c.Effective, c.Source)
		}
		c.Effective, c.Source = AppServer, "codex.auth"
	}
	return c, nil
}

// GoalAutoResumeKey is the config path of the goal auto-resume switch
// (`goal.auto_resume`): whether a restarted goal coordinator continues its
// still-eligible goal without a user turn. On by default. Captured once at
// daemon construction, like codex.control.
const GoalAutoResumeKey = "goal.auto_resume"

func goalSection(top map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if b, ok := top["goal"]; ok {
		return object(b, core.ConfigPath()+": goal")
	}
	return map[string]json.RawMessage{}, nil
}

// GoalAutoResume returns the saved switch, or set=false when unconfigured (the
// default is on). It describes the saved setting only.
func GoalAutoResume() (value, set bool, err error) {
	top, err := Read()
	if err != nil {
		return false, false, err
	}
	goal, err := goalSection(top)
	if err != nil {
		return false, false, err
	}
	b, ok := goal["auto_resume"]
	if !ok {
		return false, false, nil
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return false, false, fmt.Errorf("%s: %s must be true or false", core.ConfigPath(), GoalAutoResumeKey)
	}
	return value, true, nil
}

// ResolveGoalAutoResume is the effective switch: the saved value, or on.
func ResolveGoalAutoResume() (bool, error) {
	value, set, err := GoalAutoResume()
	if err != nil {
		return false, err
	}
	return !set || value, nil
}

// ParseGoalAutoResume accepts the spellings `amux config set` takes for the
// switch.
func ParseGoalAutoResume(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s must be on or off", GoalAutoResumeKey)
}

// SetGoalAutoResume saves the switch; a nil value unsets it (back to on).
func SetGoalAutoResume(value *bool) error {
	top, err := Read()
	if err != nil {
		return err
	}
	goal, err := goalSection(top)
	if err != nil {
		return err
	}
	if value == nil {
		delete(goal, "auto_resume")
	} else {
		goal["auto_resume"], _ = json.Marshal(*value)
	}
	if len(goal) == 0 {
		delete(top, "goal")
	} else {
		top["goal"], _ = json.Marshal(goal)
	}
	return Write(top)
}
