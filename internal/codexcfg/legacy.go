package codexcfg

import (
	"os"

	"amux/internal/accounts"
	"amux/internal/amuxcfg"
)

// A migrated workload keeps its pre-central login only while that login can
// still authenticate a new process; otherwise its launch uses central.
func init() { accounts.LegacyUsable["codex"] = legacyUsable }

func legacyUsable(source string) bool {
	switch source {
	case amuxcfg.AuthHost:
		fi, err := os.Stat(UserHome().AuthPath())
		return err == nil && fi.Mode().IsRegular()
	case amuxcfg.AuthEnvironment:
		return true // credentials come from the inherited environment
	}
	return false
}
