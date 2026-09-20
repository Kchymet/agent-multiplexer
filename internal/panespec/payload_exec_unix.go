//go:build linux || darwin

package panespec

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// init provides the final payload boundary for every protected pane. Bubblewrap
// retains descriptors explicitly inherited by its own process, so a private
// mount and PID namespace alone cannot revoke an already-open host file. Marking
// everything above stderr close-on-exec immediately before the payload exec
// closes those capabilities without disturbing bwrap's setup or supervision.
func init() {
	if os.Getenv(payloadExecEnv) != "1" || len(os.Args) < 3 || os.Args[1] != payloadExecArg {
		return
	}
	_ = os.Unsetenv(payloadExecEnv)
	if err := closePayloadDescriptors(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amux: close inherited payload descriptors: %v\n", err)
		os.Exit(126)
	}
	payload := os.Args[2]
	if !strings.ContainsRune(payload, os.PathSeparator) {
		var err error
		payload, err = exec.LookPath(payload)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "amux: resolve protected payload: %v\n", err)
			os.Exit(126)
		}
	}
	if err := syscall.Exec(payload, os.Args[2:], os.Environ()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "amux: exec protected payload: %v\n", err)
		os.Exit(126)
	}
}
