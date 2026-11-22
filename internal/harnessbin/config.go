package harnessbin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"amux/internal/amuxcfg"
)

// Settings is the "harnesses" section of amux's config.json:
//
//	"harnesses": {
//	  "update": "off" | "notify" | "auto",   (default auto)
//	  "check_interval": "24h",
//	  "restart_sessions": "on" | "off",      (default on)
//	  "claude": {"bin": "/explicit/claude", "channel": "stable"},
//	  "codex":  {"bin": "/explicit/codex"}
//	}
type Settings struct {
	Update        string `json:"update,omitempty"`
	CheckInterval string `json:"check_interval,omitempty"`
	// RestartSessions controls whether the daemon moves running sessions onto
	// a newly activated managed version at a safe point (idle between turns),
	// resuming their conversations. Unset means on.
	RestartSessions string                  `json:"restart_sessions,omitempty"`
	Kinds           map[string]KindSettings `json:"-"`
}

// KindSettings are one harness's settings.
type KindSettings struct {
	// Bin explicitly selects the launch executable, overriding the managed
	// install and PATH (AMUX_<KIND>_BIN still wins over it).
	Bin string `json:"bin,omitempty"`
	// Channel is the release channel update/auto-update follow.
	Channel string `json:"channel,omitempty"`
}

// Update modes for the daemon's scheduled check.
const (
	UpdateOff    = "off"
	UpdateNotify = "notify"
	UpdateAuto   = "auto"
)

// DefaultUpdateMode applies when harnesses.update is unset: the daemon adopts a
// managed install for each harness the user already uses and keeps it current.
const DefaultUpdateMode = UpdateAuto

// DisableEnv turns automatic harness updates (and adoption) off for the process
// that sees it, whatever harnesses.update says. The daemon reads its own
// environment, so set it where the daemon starts, then restart the daemon.
const DisableEnv = "AMUX_NO_HARNESS_AUTO_UPDATE"

// EnvDisabled reports whether DisableEnv is set to anything other than an
// explicit false.
func EnvDisabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(DisableEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// DefaultCheckInterval is how often the daemon checks when update != off.
const DefaultCheckInterval = 24 * time.Hour

const section = "harnesses"

// LoadSettings reads the harnesses section; a missing section is the zero value.
func LoadSettings() (Settings, error) {
	top, err := amuxcfg.Read()
	if err != nil {
		return Settings{}, err
	}
	return parseSettings(top[section])
}

func parseSettings(raw json.RawMessage) (Settings, error) {
	s := Settings{Kinds: map[string]KindSettings{}}
	if len(raw) == 0 {
		return s, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return s, fmt.Errorf("config harnesses: %w", err)
	}
	for field, dst := range map[string]*string{
		"update":           &s.Update,
		"check_interval":   &s.CheckInterval,
		"restart_sessions": &s.RestartSessions,
	} {
		if b, ok := m[field]; ok {
			if err := json.Unmarshal(b, dst); err != nil {
				return s, fmt.Errorf("config harnesses.%s: %w", field, err)
			}
		}
	}
	for _, k := range Kinds {
		if b, ok := m[k]; ok {
			var ks KindSettings
			if err := json.Unmarshal(b, &ks); err != nil {
				return s, fmt.Errorf("config harnesses.%s: %w", k, err)
			}
			s.Kinds[k] = ks
		}
	}
	return s, nil
}

// UpdateMode is the validated update mode: DefaultUpdateMode (auto) when
// unset, and off when the saved value is not one amux knows (a hand edit), so
// an unreadable intent never turns downloads on.
func (s Settings) UpdateMode() string {
	switch s.Update {
	case "":
		return DefaultUpdateMode
	case UpdateOff, UpdateNotify, UpdateAuto:
		return s.Update
	}
	return UpdateOff
}

// EffectiveUpdateMode is UpdateMode with the DisableEnv opt-out applied.
func (s Settings) EffectiveUpdateMode(getenv func(string) string) string {
	if EnvDisabled(getenv) {
		return UpdateOff
	}
	return s.UpdateMode()
}

// RestartSessionsEnabled reports whether running sessions move onto a newly
// activated managed version at a safe point (on unless set to off).
func (s Settings) RestartSessionsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(s.RestartSessions)) {
	case "off", "false", "no", "0":
		return false
	}
	return true
}

// Interval is the validated check interval (at least an hour).
func (s Settings) Interval() time.Duration {
	d, err := time.ParseDuration(s.CheckInterval)
	if err != nil || d <= 0 {
		return DefaultCheckInterval
	}
	if d < time.Hour {
		return time.Hour
	}
	return d
}

// Kind returns kind's settings.
func (s Settings) Kind(kind string) KindSettings { return s.Kinds[kind] }

// Setting keys accepted by Set/Get, besides harnesses.<kind>.{bin,channel}.
const (
	KeyUpdate          = "harnesses.update"
	KeyInterval        = "harnesses.check_interval"
	KeyRestartSessions = "harnesses.restart_sessions"
)

// topFields maps the top-level keys to their JSON field names.
var topFields = map[string]string{
	KeyUpdate:          "update",
	KeyInterval:        "check_interval",
	KeyRestartSessions: "restart_sessions",
}

// SettingKeys are every configurable key, for help and listing.
func SettingKeys() []string {
	keys := []string{KeyUpdate, KeyInterval, KeyRestartSessions}
	for _, k := range Kinds {
		keys = append(keys, section+"."+k+".bin", section+"."+k+".channel")
	}
	return keys
}

// IsSettingKey reports whether key is a harnesses.* setting.
func IsSettingKey(key string) bool {
	for _, k := range SettingKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// Get returns a saved setting ("" when unset).
func Get(key string) (string, error) {
	s, err := LoadSettings()
	if err != nil {
		return "", err
	}
	switch key {
	case KeyUpdate:
		return s.Update, nil
	case KeyInterval:
		return s.CheckInterval, nil
	case KeyRestartSessions:
		return s.RestartSessions, nil
	}
	for _, k := range Kinds {
		switch key {
		case section + "." + k + ".bin":
			return s.Kind(k).Bin, nil
		case section + "." + k + ".channel":
			return s.Kind(k).Channel, nil
		}
	}
	return "", fmt.Errorf("unknown setting %q", key)
}

// Set saves (value != "") or clears (value == "") a setting, validating it.
func Set(key, value string) error {
	switch key {
	case KeyUpdate:
		if value != "" && value != UpdateOff && value != UpdateNotify && value != UpdateAuto {
			return fmt.Errorf("%s must be off, notify or auto", key)
		}
	case KeyInterval:
		if value != "" {
			if d, err := time.ParseDuration(value); err != nil || d < time.Hour {
				return fmt.Errorf("%s must be a duration of at least 1h (e.g. 24h)", key)
			}
		}
	case KeyRestartSessions:
		if value != "" && value != "on" && value != "off" {
			return fmt.Errorf("%s must be on or off", key)
		}
	default:
		if !IsSettingKey(key) {
			return fmt.Errorf("unknown setting %q", key)
		}
	}
	top, err := amuxcfg.Read()
	if err != nil {
		return err
	}
	m := map[string]json.RawMessage{}
	if raw, ok := top[section]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return fmt.Errorf("config harnesses: expected an object")
		}
	}
	setField := func(obj map[string]json.RawMessage, field string) {
		if value == "" {
			delete(obj, field)
			return
		}
		b, _ := json.Marshal(value)
		obj[field] = b
	}
	if field, ok := topFields[key]; ok {
		setField(m, field)
	} else {
		for _, k := range Kinds {
			for _, field := range []string{"bin", "channel"} {
				if key != section+"."+k+"."+field {
					continue
				}
				obj := map[string]json.RawMessage{}
				if raw, ok := m[k]; ok {
					if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
						return fmt.Errorf("config harnesses.%s: expected an object", k)
					}
				}
				setField(obj, field)
				if len(obj) == 0 {
					delete(m, k)
				} else {
					b, _ := json.Marshal(obj)
					m[k] = b
				}
			}
		}
	}
	if len(m) == 0 {
		delete(top, section)
	} else {
		b, _ := json.Marshal(m)
		top[section] = b
	}
	return amuxcfg.Write(top)
}
