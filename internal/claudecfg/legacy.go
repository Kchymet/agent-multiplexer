package claudecfg

import (
	"os"
	"path/filepath"
	"runtime"

	"amux/internal/accounts"
	"amux/internal/amuxcfg"
)

// A migrated workload keeps its pre-central login only while that login can
// still authenticate a new process; otherwise its launch uses central.
func init() { accounts.LegacyUsable["claude"] = legacyUsable }

func legacyUsable(source string) bool {
	switch source {
	case amuxcfg.AuthHost:
		if runtime.GOOS == "darwin" {
			return true // the Keychain broker serves the host login
		}
		fi, err := os.Stat(filepath.Join(CredentialDirectory(), CredentialsFile))
		return err == nil && fi.Mode().IsRegular()
	case amuxcfg.AuthShared:
		return SharedAuthEnabled()
	case amuxcfg.AuthEnvironment:
		return true // credentials come from settings and the inherited environment
	}
	return false
}
