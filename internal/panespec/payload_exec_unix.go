//go:build linux || darwin

package panespec

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"amux/internal/centralauth"
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
	if os.Args[2] == centralauth.LauncherArg {
		// Central Claude: fetch only now, after descriptors are close-on-exec.
		argv := os.Args[3:]
		if len(argv) == 0 || !strings.HasPrefix(argv[0], centralauth.LaunchArgPrefix) {
			// Every daemon-started pane carries its own account launch; without
			// one delivery is refused, so fail fast instead of retrying forever.
			_, _ = fmt.Fprintln(os.Stderr, "amux: this Claude pane started without a credential launch; restart the workload with the current amux.")
			os.Exit(75)
		}
		fetch := centralauth.FetchLaunchCredentialFromSession(strings.TrimPrefix(argv[0], centralauth.LaunchArgPrefix))
		os.Exit(centralauth.LaunchCredential(argv[1:], os.Stderr, fetch))
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
