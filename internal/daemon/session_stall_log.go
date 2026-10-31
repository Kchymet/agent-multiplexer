package daemon

import (
	"context"
	"log"
	"sync"
	"time"

	"amux/internal/sessionrpc"
)

// slowRequestLog writes the daemon's slow mailbox request lines. A request
// whose own processing was slow is always logged. One that was only queued a
// long time is logged at most once per subject per queueLogInterval: when a
// stall ends, a mailbox's whole backlog reports at once, and one line with a
// count says as much as fifty.
type slowRequestLog struct {
	now   func() time.Time
	logf  func(string, ...any)
	mu    sync.Mutex
	last  map[string]time.Time
	quiet map[string]int
}

const queueLogInterval = time.Second

func newSlowRequestLog() *slowRequestLog {
	return &slowRequestLog{now: time.Now, logf: log.Printf, last: map[string]time.Time{}, quiet: map[string]int{}}
}

func (l *slowRequestLog) report(t sessionrpc.RequestTiming) {
	r := func(d time.Duration) time.Duration { return d.Round(time.Millisecond) }
	if t.Total < sessionrpc.SlowRequestThreshold {
		l.mu.Lock()
		now := l.now()
		if last, ok := l.last[t.SubjectID]; ok && now.Sub(last) < queueLogInterval {
			l.quiet[t.SubjectID]++
			l.mu.Unlock()
			return
		}
		more := l.quiet[t.SubjectID]
		l.last[t.SubjectID], l.quiet[t.SubjectID] = now, 0
		l.mu.Unlock()
		l.logf("session RPC %s: request %s %s %s waited %v before processing, then took %v (status %s %s; %d more such requests since the last line)",
			t.SubjectID, t.RequestID, t.Kind, t.Verb, r(t.Age), r(t.Total), t.Status, t.Code, more)
		return
	}
	l.logf("session RPC %s: slow request %s %s %s took %v (age %v; reserve %v, verify %v, authorize %v, dispatch %v, sign %v, publish %v; authority lock wait %v; status %s %s)",
		t.SubjectID, t.RequestID, t.Kind, t.Verb, r(t.Total), r(t.Age), r(t.Reserve), r(t.Verify), r(t.Authorize),
		r(t.Dispatch), r(t.Sign), r(t.Publish), r(t.LockWait), t.Status, t.Code)
}

// loopWatchdog reports a stalled session RPC serving loop while it is still
// stuck. The loop runs reconcile and starts every mailbox's passes on one
// goroutine, so while it is held no request in any mailbox is claimed; a
// request's own timing never sees that wait. The loop marks the phase it is
// in; a phase still running after loopStallAfter is logged once, and its end
// is logged with the full duration. A nil watchdog does nothing.
type loopWatchdog struct {
	now      func() time.Time
	logf     func(string, ...any)
	mu       sync.Mutex
	phase    string
	since    time.Time
	reported bool
}

const loopStallAfter = time.Second

func newLoopWatchdog() *loopWatchdog { return &loopWatchdog{now: time.Now, logf: log.Printf} }

// enter marks the start of phase, ending the one before it.
func (w *loopWatchdog) enter(phase string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.endLocked()
	w.phase, w.since = phase, w.now()
}

// leave marks the loop idle (waiting for work).
func (w *loopWatchdog) leave() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.endLocked()
}

func (w *loopWatchdog) endLocked() {
	if w.phase != "" && w.reported {
		w.logf("session RPC: serving loop resumed after %v in %s", w.now().Sub(w.since).Round(time.Millisecond), w.phase)
	}
	w.phase, w.reported = "", false
}

// check logs a phase that has run for loopStallAfter, once.
func (w *loopWatchdog) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.phase == "" || w.reported {
		return
	}
	if held := w.now().Sub(w.since); held >= loopStallAfter {
		w.reported = true
		w.logf("session RPC: serving loop stalled in %s for %v; no mailbox request is being claimed", w.phase, held.Round(time.Millisecond))
	}
}

// watch runs check until ctx ends.
func (w *loopWatchdog) watch(ctx context.Context, every time.Duration) {
	if w == nil {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.check()
		}
	}
}
