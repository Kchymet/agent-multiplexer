package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"amux/internal/daemonstatus"
)

// fakeProgress is a lifecycleProgress whose status file, liveness and clock
// are under the test's control.
func fakeProgress(tty bool, status *daemonstatus.Status) (*lifecycleProgress, *bytes.Buffer) {
	var out bytes.Buffer
	start := time.UnixMilli(1_000_000)
	return &lifecycleProgress{
		out:    &out,
		tty:    tty,
		width:  func() int { return 0 },
		label:  "daemon stopping",
		op:     daemonstatus.OpStopping,
		start:  start,
		budget: 54 * time.Second,
		quiet:  time.Second,
		read: func() (daemonstatus.Status, bool) {
			if status == nil || status.Op == "" {
				return daemonstatus.Status{}, false
			}
			return *status, true
		},
		alive: func(pid int) bool { return pid == 42 },
	}, &out
}

func busyStatus(now time.Time) daemonstatus.Status {
	return daemonstatus.Status{PID: 42, Op: daemonstatus.OpStopping, Phase: daemonstatus.PhaseAgents, Agents: []daemonstatus.Agent{
		{ID: "a1", Title: "fix login", State: daemonstatus.AgentBusy, Until: now.Add(20 * time.Second).UnixMilli()},
		{ID: "a2", State: daemonstatus.AgentStopped},
	}}
}

func TestLifecycleProgressTTYRewritesOneLine(t *testing.T) {
	status := daemonstatus.Status{}
	p, out := fakeProgress(true, &status)
	t0 := p.start

	p.update(t0.Add(500 * time.Millisecond)) // inside the quiet window
	if out.Len() != 0 {
		t.Fatalf("printed during the quiet window: %q", out.String())
	}
	status = busyStatus(t0.Add(2 * time.Second))
	p.update(t0.Add(2 * time.Second))
	status.Agents[0].State = daemonstatus.AgentTerminating
	status.Agents[0].Until = t0.Add(6 * time.Second).UnixMilli()
	p.update(t0.Add(3 * time.Second))
	p.finish()

	got := out.String()
	if strings.Contains(got, "\n") {
		t.Fatalf("TTY output contains newlines: %q", got)
	}
	frames := strings.Split(got, "\r\x1b[K")
	if len(frames) != 4 || frames[3] != "" {
		t.Fatalf("want two redraws then a clear, got %q", frames)
	}
	if want := "daemon stopping [2s elapsed, 52s left]: waiting on 1 agent (1 busy) — SIGTERM in 20s: fix login (busy)"; frames[1] != want {
		t.Fatalf("first frame = %q\nwant         %q", frames[1], want)
	}
	if !strings.Contains(frames[2], "1 terminating") || !strings.Contains(frames[2], "SIGKILL in 3s") {
		t.Fatalf("second frame = %q", frames[2])
	}
}

func TestLifecycleProgressTTYTruncatesToWidth(t *testing.T) {
	status := busyStatus(time.UnixMilli(1_000_000))
	p, out := fakeProgress(true, &status)
	p.width = func() int { return 40 }
	p.update(p.start.Add(2 * time.Second))
	line := strings.TrimPrefix(out.String(), "\r\x1b[K")
	if n := len([]rune(line)); n >= 40 || !strings.HasSuffix(line, "…") {
		t.Fatalf("line of %d runes not truncated below width 40: %q", n, line)
	}
}

func TestLifecycleProgressPlainPrintsOnlyChanges(t *testing.T) {
	status := daemonstatus.Status{}
	p, out := fakeProgress(false, &status)
	t0 := p.start

	p.update(t0.Add(2 * time.Second)) // no status yet: generic line
	p.update(t0.Add(2100 * time.Millisecond))
	status = busyStatus(t0)
	status.Updated = t0.Add(2 * time.Second).UnixMilli()
	for i := 0; i < 20; i++ { // the same state polled repeatedly prints once
		p.update(t0.Add(3*time.Second + time.Duration(i)*100*time.Millisecond))
	}
	status.Agents[0].State = daemonstatus.AgentTerminating
	p.update(t0.Add(6 * time.Second))
	p.finish()

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 plain lines, got %d: %q", len(lines), lines)
	}
	if !strings.HasSuffix(lines[0], "waiting for the daemon to exit") ||
		!strings.Contains(lines[1], "1 busy") || !strings.Contains(lines[2], "1 terminating") {
		t.Fatalf("plain lines = %q", lines)
	}
	if strings.ContainsAny(out.String(), "\r\x1b") {
		t.Fatalf("plain output carries terminal control: %q", out.String())
	}
}

// Mailbox reconciliation changes its counter constantly; a log gets it at most
// once per heartbeat rather than once per mailbox.
func TestLifecycleProgressPlainThrottlesCounters(t *testing.T) {
	status := daemonstatus.Status{PID: 42, Op: daemonstatus.OpStarting, Phase: daemonstatus.PhaseReconciling, Total: 100}
	p, out := fakeProgress(false, &status)
	p.op, p.label = daemonstatus.OpStarting, "daemon starting"
	t0 := p.start
	for i := 0; i <= 100; i++ {
		status.Done = i
		status.Updated = t0.Add(time.Duration(i) * 150 * time.Millisecond).UnixMilli()
		p.update(t0.Add(2*time.Second + time.Duration(i)*150*time.Millisecond))
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 || len(lines) > 3 {
		t.Fatalf("15s of counting printed %d lines, want 2-3: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "reconciling session mailboxes") {
		t.Fatalf("first line = %q", lines[0])
	}
}

func TestLifecycleProgressIgnoresForeignStatus(t *testing.T) {
	for name, status := range map[string]daemonstatus.Status{
		"dead daemon": {PID: 7, Op: daemonstatus.OpStopping, Phase: daemonstatus.PhaseAgents},
		"other op":    {PID: 42, Op: daemonstatus.OpStarting, Phase: daemonstatus.PhaseReconciling},
	} {
		t.Run(name, func(t *testing.T) {
			p, out := fakeProgress(false, &status)
			p.update(p.start.Add(2 * time.Second))
			if !strings.HasSuffix(strings.TrimSpace(out.String()), "waiting for the daemon to exit") {
				t.Fatalf("foreign status was rendered: %q", out.String())
			}
		})
	}
}

func TestLifecycleProgressNilIsSilent(t *testing.T) {
	var p *lifecycleProgress
	p.update(time.Now())
	p.finish()
}
