//go:build !linux

package procstat

import "time"

// threadCPU is unavailable off Linux (no per-thread rusage, and no procfs to
// sample anyway).
func threadCPU() (time.Duration, bool) { return 0, false }
