package amuxcfg

import (
	"encoding/json"
	"fmt"
	"strings"

	"amux/internal/core"
)

// AutoUpdateKey is the config path of the automatic self-update switch
// (`update.auto`): whether the daemon periodically installs new amux releases.
// On by default; AMUX_NO_AUTO_UPDATE also turns it off. Unlike most daemon
// settings it is re-read on every check, so a change applies without a restart.
const AutoUpdateKey = "update.auto"

func updateSection(top map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if b, ok := top["update"]; ok {
		return object(b, core.ConfigPath()+": update")
	}
	return map[string]json.RawMessage{}, nil
}

// AutoUpdate returns the saved switch, or set=false when unconfigured.
func AutoUpdate() (value, set bool, err error) {
	top, err := Read()
	if err != nil {
		return false, false, err
	}
	sec, err := updateSection(top)
	if err != nil {
		return false, false, err
	}
	b, ok := sec["auto"]
	if !ok {
		return false, false, nil
	}
	if err := json.Unmarshal(b, &value); err != nil {
		return false, false, fmt.Errorf("%s: %s must be true or false", core.ConfigPath(), AutoUpdateKey)
	}
	return value, true, nil
}

// ResolveAutoUpdate is the effective saved switch: the saved value, or on.
func ResolveAutoUpdate() (bool, error) {
	value, set, err := AutoUpdate()
	if err != nil {
		return false, err
	}
	return !set || value, nil
}

// ParseOnOff accepts the spellings `amux config set` takes for a switch.
func ParseOnOff(key, s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	}
	return false, fmt.Errorf("%s must be on or off", key)
}

// SetAutoUpdate saves the switch; nil unsets it (back to on).
func SetAutoUpdate(value *bool) error {
	top, err := Read()
	if err != nil {
		return err
	}
	sec, err := updateSection(top)
	if err != nil {
		return err
	}
	if value == nil {
		delete(sec, "auto")
	} else {
		sec["auto"], _ = json.Marshal(*value)
	}
	if len(sec) == 0 {
		delete(top, "update")
	} else {
		top["update"], _ = json.Marshal(sec)
	}
	return Write(top)
}
