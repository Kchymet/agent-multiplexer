//go:build !linux

package directsvc

import "os/exec"

// configureChild has no parent-death signal outside Linux; the child's parent
// watch (WatchParent) is what stops it when the daemon dies.
func configureChild(*exec.Cmd) {}
