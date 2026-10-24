//go:build !linux

package codexauth

import "syscall"

// sysProcAttr puts the native server in its own process group (stopped as a
// unit).
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
