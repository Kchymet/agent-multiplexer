package harnessbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// runningExecutables lists the executable path of every visible process. On
// Linux it reads /proc/<pid>/exe (sandboxed panes mount the version directory
// at its host path, so the path matches); on macOS it asks ps for each
// process's executable. An error means the list may be incomplete and GC must
// not trust it.
func runningExecutables() ([]string, error) {
	if runtime.GOOS == "linux" {
		procs, err := filepath.Glob("/proc/[0-9]*")
		if err != nil {
			return nil, err
		}
		if len(procs) == 0 {
			return nil, os.ErrNotExist
		}
		var out []string
		for _, p := range procs {
			exe, err := os.Readlink(filepath.Join(p, "exe"))
			if err != nil {
				continue // kernel threads, exited processes, other users
			}
			out = append(out, strings.TrimSuffix(exe, " (deleted)"))
		}
		return out, nil
	}
	b, err := exec.Command("ps", "-axo", "comm=").Output()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}
