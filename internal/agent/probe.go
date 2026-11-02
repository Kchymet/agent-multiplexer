package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ProbeResult is local diagnostic information. A successful version invocation
// establishes executable readiness, not authentication or model availability.
type ProbeResult struct {
	Kind, Path, Version string
	Err                 error
}

// Probe uses the launcher's resolver and binary override, in the calling
// process's environment. Providers call this on the host, never in an agent's
// private configuration home. Each probe has a bounded total runtime/output.
func Probe(ctx context.Context, kind string) ProbeResult {
	r := ProbeResult{Kind: kind}
	if kind == "" || !Known(kind) {
		r.Err = fmt.Errorf("unknown harness %q", kind)
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r.Path = resolveContext(ctx, envOr("AMUX_"+strings.ToUpper(kind)+"_BIN", kind))
	if !installed(r.Path) {
		r.Err = fmt.Errorf("%s: %w", r.Path, ErrNotInstalled)
		return r
	}
	cmd := exec.CommandContext(ctx, r.Path, "--version")
	cmd.WaitDelay = 100 * time.Millisecond
	var out probeOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		r.Err = fmt.Errorf("version check failed: %w", err)
		return r
	}
	r.Version = strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0])
	if r.Version == "" {
		r.Err = fmt.Errorf("version check returned no version")
	}
	return r
}

// ErrNotInstalled marks a probe whose executable does not exist on this host —
// the ordinary state of an optional harness (Hermes on a Claude/Codex machine),
// as opposed to an installed CLI that failed its version check. Callers test it
// with errors.Is.
var ErrNotInstalled = errors.New("not installed")

// installed reports whether a resolved launcher path names something that
// exists. resolveContext hands back the bare name when no lookup found it, and
// an explicit path (an AMUX_<KIND>_BIN override) is checked as given.
func installed(path string) bool {
	if !strings.ContainsRune(path, '/') {
		_, err := exec.LookPath(path)
		return err == nil
	}
	_, err := os.Stat(path)
	return err == nil
}

type probeOutput struct{ strings.Builder }

func (b *probeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := 512 - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Builder.Write(p)
	}
	return n, nil
}
