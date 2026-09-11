//go:build linux

package panespec

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestPayloadTrampolineResolvesBareAndPreservesAbsoluteExecutables(t *testing.T) {
	for _, executable := range []string{"sh", "/bin/sh"} {
		t.Run(strings.ReplaceAll(executable, "/", "_"), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], payloadExecArg, executable, "-c", "printf payload-ok")
			cmd.Env = []string{payloadExecEnv + "=1", "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("payload trampoline %q: %v: %s", executable, err, out)
			}
			if string(out) != "payload-ok" {
				t.Fatalf("payload trampoline %q output = %q", executable, out)
			}
		})
	}
}
