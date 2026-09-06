package claudecfg

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"amux/internal/core"
)

// SecureStorageEnv separates Claude's credential store (including refresh and
// storage-write locks) from its settings and conversation directory. Claude
// recognizes this override, but it is not yet a documented public API:
// https://github.com/anthropics/claude-code/issues/79223
const SecureStorageEnv = "CLAUDE_SECURESTORAGE_CONFIG_DIR"

var credentialOverrides = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
}

// SharedAuthDir has one stable directory entry for the lifetime of all panes.
// Only its contents are replaced. No credentials are copied from the user's
// login: that would fork an existing rotating refresh-token chain.
func SharedAuthDir() string { return filepath.Join(core.AuthDir(), "claude") }

func SharedAuthEnabled() bool {
	b, err := os.ReadFile(filepath.Join(SharedAuthDir(), "enabled"))
	return err == nil && string(b) == "1\n"
}

// EnableSharedAuth is called only after Claude's interactive login succeeds.
func EnableSharedAuth() error {
	f, err := os.CreateTemp(SharedAuthDir(), ".enabled-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("1\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(SharedAuthDir(), "enabled"))
}

// Login serializes amux login commands. The OS releases the lock even when the
// terminal or process dies. Claude separately owns its credential-write and
// refresh locks inside the same directory used by all sessions.
func Login(run func() error) error {
	if err := os.MkdirAll(SharedAuthDir(), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(SharedAuthDir(), ".amux-login.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another amux Claude login is running (or its lock is unavailable): %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := run(); err != nil {
		return err
	}
	return EnableSharedAuth()
}

// AuthCommandEnv pins login/status to the same store agents use, regardless of
// which session launches the command. Remove alternate credentials so a status
// check cannot succeed using an unrelated API key or inherited token.
func AuthCommandEnv(base []string) []string {
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(credentialOverrides, key) {
			continue
		}
		switch key {
		case Env, SecureStorageEnv, "CLAUDECODE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, Env+"="+SharedAuthDir(), SecureStorageEnv+"="+SharedAuthDir())
}
