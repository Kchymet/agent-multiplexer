package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"amux/internal/sessionrpc"
)

type fakeLogClock struct {
	now   time.Time
	lines []string
}

func (c *fakeLogClock) clock() time.Time { return c.now }

func (c *fakeLogClock) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

// Queued-request lines are bounded per subject, slow-request lines per verb,
// and the line that ends a quiet period counts what it replaced.
func TestSlowRequestLogRateLimitsPerSubjectAndVerb(t *testing.T) {
	c := &fakeLogClock{now: time.Unix(1_800_000_000, 0)}
	l := newSlowRequestLog()
	l.rate.now, l.rate.logf = c.clock, c.logf
	queued := func(subject string) sessionrpc.RequestTiming {
		return sessionrpc.RequestTiming{SubjectID: subject, Verb: "report-context", Age: 9 * time.Second, Total: time.Millisecond}
	}
	for i := 0; i < 50; i++ {
		l.report(queued("a"))
	}
	l.report(queued("b")) // a's backlog does not hide b's stall
	for i := 0; i < 3; i++ {
		l.report(sessionrpc.RequestTiming{SubjectID: "a", Verb: "slow", Total: 2 * time.Second})
	}
	l.report(sessionrpc.RequestTiming{SubjectID: "a", Verb: "other", Total: 2 * time.Second})
	c.now = c.now.Add(queueLogInterval)
	l.report(queued("a"))
	if len(c.lines) != 5 {
		t.Fatalf("lines = %q, want 5", c.lines)
	}
	for i, want := range []string{"session RPC a: request", "session RPC b: request", " slow took 2s", " other took 2s", "49 similar lines suppressed in the last 10s"} {
		if !strings.Contains(c.lines[i], want) {
			t.Errorf("line %d = %q, want it to contain %q", i, c.lines[i], want)
		}
	}
	if strings.Contains(c.lines[0], "suppressed") {
		t.Errorf("first line reports suppression: %q", c.lines[0])
	}
}

// Client-chosen keys cannot grow the maps without bound: stale keys are
// pruned, and while every key is live a new one shares a rate-limited
// overflow key.
func TestRateLogBoundsItsKeys(t *testing.T) {
	c := &fakeLogClock{now: time.Unix(1_800_000_000, 0)}
	l := newRateLog(stallLogInterval)
	l.now, l.logf = c.clock, c.logf
	for i := 0; i < 10*rateLogMaxKeys; i++ {
		l.printf(fmt.Sprintf("slow:verb%d", i), "line %d", i)
	}
	if len(l.last) > rateLogMaxKeys+1 || len(l.quiet) > rateLogMaxKeys+1 {
		t.Fatalf("tracked %d/%d keys, want at most %d", len(l.last), len(l.quiet), rateLogMaxKeys+1)
	}
	// rateLogMaxKeys distinct lines, then one overflow line for the rest.
	if len(c.lines) != rateLogMaxKeys+1 {
		t.Fatalf("lines = %d, want %d", len(c.lines), rateLogMaxKeys+1)
	}
	c.now = c.now.Add(stallLogInterval)
	l.printf("slow:fresh", "fresh")
	if _, ok := l.last["slow:fresh"]; !ok || len(l.last) > 2 {
		t.Fatalf("stale keys not pruned: %d tracked, fresh tracked=%v", len(l.last), ok)
	}
}

func TestRateLogBoundsEachKey(t *testing.T) {
	c := &fakeLogClock{now: time.Unix(1_800_000_000, 0)}
	l := newRateLog(stallLogInterval)
	l.now, l.logf = c.clock, c.logf
	for i := 0; i < 1000; i++ {
		l.printf("slow-reconcile", "slow reconcile took %dms", i)
		c.now = c.now.Add(50 * time.Millisecond) // one per reconcile tick
	}
	// 50s of ticks: one line per 10s.
	if len(c.lines) != 5 {
		t.Fatalf("lines = %d (%q), want 5", len(c.lines), c.lines)
	}
	if !strings.Contains(c.lines[1], "[199 similar lines suppressed in the last 10s]") {
		t.Fatalf("second line = %q", c.lines[1])
	}
}

func TestLoopWatchdogReportsStallWhileHeldAndOnResume(t *testing.T) {
	c := &fakeLogClock{now: time.Unix(1_800_000_000, 0)}
	w := newLoopWatchdog()
	w.now, w.logf = c.clock, c.logf

	w.enter("reconcile")
	c.now = c.now.Add(loopStallAfter / 2)
	w.check()
	w.enter("serve") // a short reconcile is not a stall
	c.now = c.now.Add(loopStallAfter / 2)
	w.leave()
	w.check()
	if len(c.lines) != 0 {
		t.Fatalf("short phases logged: %q", c.lines)
	}

	w.enter("reconcile")
	c.now = c.now.Add(loopStallAfter)
	w.check()
	c.now = c.now.Add(9 * time.Second)
	w.check() // reported once, not on every check
	w.enter("serve")
	if len(c.lines) != 2 ||
		!strings.Contains(c.lines[0], "serving loop stalled in reconcile for 1s") ||
		!strings.Contains(c.lines[1], "serving loop resumed after 10s in reconcile") {
		t.Fatalf("lines = %q", c.lines)
	}
	var nilWatchdog *loopWatchdog
	nilWatchdog.enter("reconcile")
	nilWatchdog.leave()
}
