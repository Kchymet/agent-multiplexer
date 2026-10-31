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

func TestSlowRequestLogRateLimitsQueueWaitPerSubject(t *testing.T) {
	c := &fakeLogClock{now: time.Unix(1_800_000_000, 0)}
	l := newSlowRequestLog()
	l.now, l.logf = c.clock, c.logf
	queued := func(subject string) sessionrpc.RequestTiming {
		return sessionrpc.RequestTiming{SubjectID: subject, Verb: "report-context", Age: 9 * time.Second, Total: time.Millisecond}
	}
	for i := 0; i < 50; i++ {
		l.report(queued("a"))
	}
	l.report(queued("b"))
	l.report(sessionrpc.RequestTiming{SubjectID: "a", Verb: "slow", Total: 2 * time.Second})
	c.now = c.now.Add(queueLogInterval)
	l.report(queued("a"))
	if len(c.lines) != 4 {
		t.Fatalf("lines = %q, want 4", c.lines)
	}
	for i, want := range []string{"waited 9s before processing", "session RPC b: request", "slow request", "49 more such requests"} {
		if !strings.Contains(c.lines[i], want) {
			t.Errorf("line %d = %q, want it to contain %q", i, c.lines[i], want)
		}
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
