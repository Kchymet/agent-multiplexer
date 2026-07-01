// Package proctree resolves process ancestry so a session's process id can be
// mapped to the pane that ultimately owns it. A `claude` process is often
// a child of the pane's shell, so a direct pid==pane_pid match is not enough;
// we climb the parent chain until we hit a known pane pid.
package proctree

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// PPID returns the parent process id of pid, or 0 if it can't be determined.
func PPID(pid int) int {
	if runtime.GOOS == "linux" {
		// Fast path: read /proc/<pid>/stat. Field 4 is ppid, but the comm field
		// (2) may contain spaces/parens, so parse from the last ')'.
		if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			s := string(b)
			if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
				fields := strings.Fields(s[i+2:])
				if len(fields) >= 2 {
					if ppid, err := strconv.Atoi(fields[1]); err == nil {
						return ppid
					}
				}
			}
		}
		return 0
	}
	// Portable fallback (macOS and anything without procfs).
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return ppid
}

// WindowFor climbs from pid through its ancestors looking for any pid present
// in paneWindows (pane_pid -> window_id). Returns the window id, or "" if the
// process isn't running under a pane in the isolated server.
func WindowFor(pid int, paneWindows map[int]string) string {
	cur := pid
	for i := 0; i < 16 && cur > 1; i++ {
		if w, ok := paneWindows[cur]; ok {
			return w
		}
		cur = PPID(cur)
	}
	return ""
}
