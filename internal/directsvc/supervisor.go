package directsvc

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Process is one running managed child.
type Process interface {
	Pid() int
	// Wait blocks until the child exits. Its error should carry the child's last
	// line of output, since that is usually the reason (a refused bind, a busy
	// store, an unreadable certificate).
	Wait() error
	// Terminate asks the child to stop (SIGTERM); Kill forces it.
	Terminate() error
	Kill() error
}

// Starter launches the child for a configuration.
type Starter func(Settings) (Process, error)

// Supervisor keeps the managed direct listener in step with the saved setting
// for as long as the daemon runs: started when enabled, restarted with backoff
// when it exits, restarted when its configuration (or certificate) changes,
// and stopped when the setting is turned off or the daemon stops.
//
// It polls the setting rather than taking a daemon RPC, so `amux direct
// enable|disable` is a write to config.json plus a wait on the status file —
// the same file a reader would use to check the outcome — and there is no new
// privileged daemon surface.
type Supervisor struct {
	Load        func() (Settings, bool, error)
	Fingerprint func(Settings) string
	Start       Starter
	StatusPath  string
	Logf        func(string, ...any)
	Now         func() time.Time

	Poll         time.Duration // how often the setting is re-read (default 2s)
	StopGrace    time.Duration // SIGTERM → SIGKILL (default 5s)
	BackoffMin   time.Duration // default 1s
	BackoffMax   time.Duration // default 1m
	HealthyAfter time.Duration // a child that ran this long resets the backoff (default 1m)
}

// NewSupervisor is the production supervisor: it reads config.json, writes
// StatusPath, and starts `<self> direct serve --managed`.
func NewSupervisor(self string, logf func(string, ...any)) *Supervisor {
	return &Supervisor{
		Load:        Load,
		Fingerprint: Fingerprint,
		Start:       ExecStarter(self, LogPath()),
		StatusPath:  StatusPath(),
		Logf:        logf,
	}
}

func (s *Supervisor) defaults() {
	if s.Fingerprint == nil {
		s.Fingerprint = Fingerprint
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Poll <= 0 {
		s.Poll = 2 * time.Second
	}
	if s.StopGrace <= 0 {
		s.StopGrace = 5 * time.Second
	}
	if s.BackoffMin <= 0 {
		s.BackoffMin = time.Second
	}
	if s.BackoffMax <= 0 {
		s.BackoffMax = time.Minute
	}
	if s.HealthyAfter <= 0 {
		s.HealthyAfter = time.Minute
	}
}

type child struct {
	proc      Process
	fp        string
	startedAt time.Time
	exited    chan error
}

// Run supervises until ctx ends, then stops the child and records StateStopped.
func (s *Supervisor) Run(ctx context.Context) {
	s.defaults()
	var (
		cur       *child
		backoff   = s.BackoffMin
		nextStart time.Time
		restarts  int
		lastErr   string
		lastFP    string
	)
	st := Status{DaemonPID: os.Getpid()}
	publish := func() {
		st.Restarts, st.LastError, st.UpdatedAt = restarts, lastErr, s.Now()
		st.PID, st.StartedAt = 0, time.Time{}
		if cur != nil {
			st.PID, st.StartedAt = cur.proc.Pid(), cur.startedAt
		}
		if err := writeStatus(s.StatusPath, st); err != nil {
			s.Logf("direct: write status: %v", err)
		}
	}
	stop := func(why string) {
		if cur == nil {
			return
		}
		s.Logf("direct: stopping managed listener pid %d (%s)", cur.proc.Pid(), why)
		_ = cur.proc.Terminate()
		t := time.NewTimer(s.StopGrace)
		select {
		case <-cur.exited:
		case <-t.C:
			s.Logf("direct: pid %d ignored SIGTERM for %s; killing", cur.proc.Pid(), s.StopGrace)
			_ = cur.proc.Kill()
			<-cur.exited
		}
		t.Stop()
		cur = nil
	}

	tick := time.NewTicker(s.Poll)
	defer tick.Stop()
	for {
		now := s.Now()
		cfg, _, err := s.Load()
		switch {
		case err != nil:
			// A setting that cannot be read is never taken as permission to keep
			// a listener up; fix the file and the next poll recovers.
			stop("setting unreadable")
			st.State, st.Enabled, st.Fingerprint, st.NextRetry = StateInvalid, false, "", time.Time{}
			lastErr = err.Error()
		case !cfg.Enabled:
			stop("disabled")
			if st.State != StateDisabled {
				backoff, nextStart, lastErr = s.BackoffMin, time.Time{}, ""
			}
			st.State, st.Enabled, st.Fingerprint, st.NextRetry = StateDisabled, false, "", time.Time{}
		default:
			fp := s.Fingerprint(cfg)
			st.Enabled, st.Fingerprint = true, fp
			if cur != nil && cur.fp != fp {
				stop("configuration changed")
			}
			if fp != lastFP {
				// A new configuration deserves a prompt first attempt, whatever
				// the previous one's failures left the backoff at.
				backoff, nextStart, lastFP = s.BackoffMin, time.Time{}, fp
			}
			if cur == nil && !now.Before(nextStart) {
				proc, err := s.Start(cfg)
				if err != nil {
					lastErr = err.Error()
					nextStart, backoff = now.Add(backoff), nextBackoff(backoff, s.BackoffMax)
					s.Logf("direct: start managed listener: %v (retry in %s)", err, nextStart.Sub(now).Round(time.Second))
				} else {
					cur = &child{proc: proc, fp: fp, startedAt: now, exited: make(chan error, 1)}
					go func(c *child) { c.exited <- c.proc.Wait() }(cur)
					s.Logf("direct: managed listener started (pid %d, %s)", proc.Pid(), cfg.Listen)
				}
			}
			if cur != nil {
				st.State, st.NextRetry = StateRunning, time.Time{}
			} else {
				st.State, st.NextRetry = StateBackoff, nextStart
			}
		}
		publish()

		var exited chan error
		if cur != nil {
			exited = cur.exited
		}
		select {
		case <-ctx.Done():
			stop("daemon stopping")
			st.State, st.NextRetry = StateStopped, time.Time{}
			publish()
			return
		case err := <-exited:
			ran := s.Now().Sub(cur.startedAt)
			if ran >= s.HealthyAfter {
				backoff = s.BackoffMin
			}
			lastErr = "exited"
			if err != nil {
				lastErr = err.Error()
			}
			restarts++
			nextStart = s.Now().Add(backoff)
			s.Logf("direct: managed listener pid %d exited after %s: %s (retry in %s)",
				cur.proc.Pid(), ran.Round(time.Second), lastErr, backoff)
			backoff = nextBackoff(backoff, s.BackoffMax)
			cur = nil
		case <-tick.C:
		}
	}
}

func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

// String renders a status for humans.
func (st Status) String() string {
	switch st.State {
	case StateRunning:
		return fmt.Sprintf("running (pid %d, since %s)", st.PID, st.StartedAt.Format(time.RFC3339))
	case StateBackoff:
		return fmt.Sprintf("restarting (next attempt %s)", st.NextRetry.Format(time.RFC3339))
	case "":
		return "unknown"
	}
	return st.State
}
