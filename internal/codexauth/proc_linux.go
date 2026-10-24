package codexauth

import "syscall"

// sysProcAttr puts the native server in its own process group (stopped as a
// unit) and kills it if the daemon thread that started it dies.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
