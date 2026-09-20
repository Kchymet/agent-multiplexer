//go:build darwin

package daemon

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Darwin's default /var/folders path is a symlink and is too long for many
	// Unix sockets. Keep fixtures canonical without weakening no-follow checks.
	if err := os.Setenv("TMPDIR", "/private/tmp"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
