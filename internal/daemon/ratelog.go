package daemon

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// rateLog writes at most one line per key every interval, and the line that
// ends a quiet period says how many it stood in for. The daemon's stall lines
// (slow reconcile, queued and slow mailbox requests, actions holding locks)
// fire per tick or per request while the daemon is loaded, which is exactly
// when a log that grows by millions of lines helps least.
type rateLog struct {
	now      func() time.Time
	logf     func(string, ...any)
	interval time.Duration
	mu       sync.Mutex
	last     map[string]time.Time
	quiet    map[string]int
}

// stallLogInterval bounds each stall line kind to one line per interval.
const stallLogInterval = 10 * time.Second

func newRateLog(interval time.Duration) *rateLog {
	return &rateLog{now: time.Now, logf: log.Printf, interval: interval, last: map[string]time.Time{}, quiet: map[string]int{}}
}

// printf logs the line unless key logged within the interval, in which case it
// only counts it. A logged line ends with the count suppressed since the last.
func (l *rateLog) printf(key, format string, args ...any) {
	l.mu.Lock()
	now := l.now()
	if last, ok := l.last[key]; ok && now.Sub(last) < l.interval {
		l.quiet[key]++
		l.mu.Unlock()
		return
	}
	suppressed, since := l.quiet[key], l.last[key]
	l.last[key], l.quiet[key] = now, 0
	l.mu.Unlock()
	line := fmt.Sprintf(format, args...)
	if suppressed > 0 {
		line += fmt.Sprintf(" [%d similar lines suppressed in the last %v]", suppressed, now.Sub(since).Round(time.Second))
	}
	l.logf("%s", line)
}

// stallLog rate-limits the daemon's own stall lines (actions holding locks,
// slow reconcile passes).
var stallLog = newRateLog(stallLogInterval)
