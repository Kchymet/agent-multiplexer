//go:build darwin

package main

import (
	"os"
)

// canonicalDarwinTempDir is preferred over Darwin's default /var/folders path:
// that one is a symlink and is too long for many Unix sockets. Keeping fixtures
// canonical avoids weakening no-follow checks.
const canonicalDarwinTempDir = "/private/tmp"

// useCanonicalTestTempDir points TMPDIR at the canonical directory when this
// process may write there. Inside an amux sandbox it may not, and the sandbox
// already provides a private TMPDIR; that inherited value is kept as-is rather
// than substituting any host path.
func useCanonicalTestTempDir() error {
	probe, err := os.CreateTemp(canonicalDarwinTempDir, "amux-test-probe-*")
	if err != nil {
		return nil
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return os.Setenv("TMPDIR", canonicalDarwinTempDir)
}
