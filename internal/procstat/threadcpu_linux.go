package procstat

import (
	"syscall"
	"time"
)

// rusageThread is RUSAGE_THREAD (Linux): usage of the calling thread only.
const rusageThread = 1

// threadCPU returns the calling OS thread's user+system CPU time. The caller
// must be locked to its thread (runtime.LockOSThread) for deltas to mean
// anything.
func threadCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(rusageThread, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
