//go:build race

package daemon

// raceSlowdown scales latency bounds in tests run under the race detector.
const raceSlowdown = 3
