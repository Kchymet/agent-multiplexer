package main

import (
	"amux/internal/providercfg"
	"fmt"
)

func providerAccountUseDefault(c providercfg.Config, publish, readOnly bool) bool {
	if c.AccountUseSet {
		return c.AccountUse
	}
	return c.AccountUse || publish && !readOnly
}

func rejectRemoteAccountManagement(enabled bool) error {
	if enabled {
		return fmt.Errorf("--account-manage is no longer supported; configure accounts through amux CLI/TUI (Harness account access is read-only)")
	}
	return nil
}
