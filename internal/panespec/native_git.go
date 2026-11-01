package panespec

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// appleGitShim is the Command Line Tools stub macOS installs as git. It does
// not contain Git: each run asks xcrun for the selected developer directory,
// creating a lookup cache under the per-user temporary directory
// (confstr(_CS_DARWIN_USER_TEMP_DIR), not TMPDIR). Seatbelt denies that write,
// and on macOS 26 the uncached lookup inside the sandbox can outlast any
// caller's patience, so a pane must never run the shim.
const appleGitShim = "/usr/bin/git"

// nativeGitTarget returns the real Git binary for a pane when the host's git
// is Apple's shim, resolved on the host where xcrun works. It returns "" when
// the host's git is already a real binary (Homebrew, say), which the pane then
// runs as before, or when no developer tools are installed.
func nativeGitTarget() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	path, err := exec.LookPath("git")
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if path != appleGitShim {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/xcrun", "--find", "git").Output()
	if err != nil {
		return ""
	}
	target := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if !filepath.IsAbs(target) || target == appleGitShim {
		return ""
	}
	if info, err := os.Stat(target); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return target
}

// publishNativeGit points the pane's git, first on its PATH, at the real
// binary behind Apple's shim, or removes a stale alias when none applies.
func publishNativeGit(parent string) error {
	if target := nativeGitTarget(); target != "" {
		return publishNativeAlias(parent, "git", target)
	}
	if err := os.Remove(filepath.Join(parent, "git")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
