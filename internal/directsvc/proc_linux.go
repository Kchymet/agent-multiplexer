package directsvc

import (
	"os/exec"
	"syscall"
)

// configureChild asks the kernel to SIGTERM the managed child if the daemon
// thread that started it dies, so an abruptly killed daemon cannot leave a
// listener holding the direct store lock. The child's parent watch covers the
// remaining cases (and other platforms).
func configureChild(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
