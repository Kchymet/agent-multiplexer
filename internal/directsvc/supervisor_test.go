package directsvc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc is a managed child that exits when told to (or when terminated,
// unless it is stubborn).
type fakeProc struct {
	pid        int
	cfg        Settings
	done       chan error
	once       sync.Once
	stubborn   bool
	terminated chan struct{}
	killed     chan struct{}
}

func (p *fakeProc) Pid() int    { return p.pid }
func (p *fakeProc) Wait() error { return <-p.done }
func (p *fakeProc) exit(err error) {
	p.once.Do(func() { p.done <- err })
}
func (p *fakeProc) Terminate() error {
	select {
	case <-p.terminated:
	default:
		close(p.terminated)
	}
	if !p.stubborn {
		p.exit(nil)
	}
	return nil
}
func (p *fakeProc) Kill() error {
	select {
	case <-p.killed:
	default:
		close(p.killed)
	}
	p.exit(errors.New("killed"))
	return nil
}

type harness struct {
	t        *testing.T
	mu       sync.Mutex
	cfg      Settings
	loadErr  error
	procs    []*fakeProc
	started  chan *fakeProc
	startErr error
	stubborn bool
	sup      *Supervisor
	cancel   context.CancelFunc
	done     chan struct{}
}

func newHarness(t *testing.T, cfg Settings) *harness {
	h := &harness{t: t, cfg: cfg, started: make(chan *fakeProc, 16), done: make(chan struct{})}
	h.sup = &Supervisor{
		Load: func() (Settings, bool, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.cfg, true, h.loadErr
		},
		Fingerprint: func(s Settings) string { return strings.Join(append(s.Args(), boolStr(s.Enabled)), " ") },
		Start: func(s Settings) (Process, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.startErr != nil {
				return nil, h.startErr
			}
			p := &fakeProc{pid: 1000 + len(h.procs), cfg: s, done: make(chan error, 1), stubborn: h.stubborn,
				terminated: make(chan struct{}), killed: make(chan struct{})}
			h.procs = append(h.procs, p)
			h.started <- p
			return p, nil
		},
		StatusPath:   filepath.Join(t.TempDir(), "direct-managed.json"),
		Poll:         5 * time.Millisecond,
		StopGrace:    50 * time.Millisecond,
		BackoffMin:   10 * time.Millisecond,
		BackoffMax:   40 * time.Millisecond,
		HealthyAfter: time.Hour,
	}
	return h
}

func boolStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { defer close(h.done); h.sup.Run(ctx) }()
	h.t.Cleanup(func() { cancel(); <-h.done })
}

func (h *harness) set(f func(*Settings)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.cfg)
}

func (h *harness) nextStart() *fakeProc {
	h.t.Helper()
	select {
	case p := <-h.started:
		return p
	case <-time.After(2 * time.Second):
		h.t.Fatal("managed child was not started")
		return nil
	}
}

func (h *harness) noStart(d time.Duration) {
	h.t.Helper()
	select {
	case p := <-h.started:
		h.t.Fatalf("unexpected start of pid %d", p.pid)
	case <-time.After(d):
	}
}

func (h *harness) waitStatus(pred func(Status) bool) Status {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last Status
	for time.Now().Before(deadline) {
		if st, err := ReadStatus(h.sup.StatusPath); err == nil {
			last = st
			if pred(st) {
				return st
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("status never matched; last %+v", last)
	return last
}

func waitClosed(t *testing.T, c chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

var enabledCfg = Settings{
	Enabled: true, Listen: "100.64.0.5:8443", Endpoints: []string{"https://amux.example:8443"},
	TLSCert: "/certs/c.pem", TLSKey: "/certs/k.pem",
}

// Off is the default: an absent or disabled setting never starts a listener.
func TestSupervisorDisabledStartsNothing(t *testing.T) {
	h := newHarness(t, Settings{Listen: "127.0.0.1:8443"})
	h.run()
	st := h.waitStatus(func(s Status) bool { return s.State == StateDisabled })
	if st.Enabled || st.PID != 0 || st.DaemonPID != os.Getpid() {
		t.Fatalf("disabled status = %+v", st)
	}
	h.noStart(50 * time.Millisecond)
}

func TestSupervisorStartsWhenEnabledAndStopsOnDisable(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.run()
	p := h.nextStart()
	h.waitStatus(func(s Status) bool { return s.State == StateRunning && s.PID == p.pid && s.Enabled })

	h.set(func(s *Settings) { s.Enabled = false })
	waitClosed(t, p.terminated, "terminate on disable")
	st := h.waitStatus(func(s Status) bool { return s.State == StateDisabled })
	if st.PID != 0 {
		t.Fatalf("disabled status still names pid %d", st.PID)
	}
	h.noStart(50 * time.Millisecond)

	// Turning it back on starts a fresh child promptly.
	h.set(func(s *Settings) { s.Enabled = true })
	h.nextStart()
}

func TestSupervisorRestartsCrashedChildWithBackoff(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.run()
	p1 := h.nextStart()
	p1.exit(errors.New("exit status 1: another `amux direct serve` already owns the direct store"))
	st := h.waitStatus(func(s Status) bool { return s.Restarts == 1 })
	if !strings.Contains(st.LastError, "already owns the direct store") {
		t.Fatalf("last error = %q, want the child's reason", st.LastError)
	}
	p2 := h.nextStart()
	if p2.pid == p1.pid {
		t.Fatal("restart reused the old process")
	}
	h.waitStatus(func(s Status) bool { return s.State == StateRunning && s.PID == p2.pid })
}

func TestSupervisorBacksOffWhenStartFails(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.startErr = errors.New("exec: no such file")
	h.run()
	st := h.waitStatus(func(s Status) bool { return s.State == StateBackoff })
	if st.LastError != "exec: no such file" || st.NextRetry.IsZero() {
		t.Fatalf("backoff status = %+v", st)
	}
	h.mu.Lock()
	h.startErr = nil
	h.mu.Unlock()
	h.nextStart()
}

// A changed configuration (or a renewed certificate, via the fingerprint)
// replaces the running child rather than leaving it on the old settings.
func TestSupervisorRestartsOnConfigChange(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.run()
	p1 := h.nextStart()
	h.set(func(s *Settings) { s.Endpoints = []string{"https://other.example:8443"} })
	waitClosed(t, p1.terminated, "terminate on config change")
	p2 := h.nextStart()
	if got := p2.cfg.Endpoints; len(got) != 1 || got[0] != "https://other.example:8443" {
		t.Fatalf("restarted child got endpoints %v", got)
	}
}

func TestSupervisorKillsChildThatIgnoresTerminate(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.stubborn = true
	h.run()
	p := h.nextStart()
	h.set(func(s *Settings) { s.Enabled = false })
	waitClosed(t, p.terminated, "terminate")
	waitClosed(t, p.killed, "kill after grace")
	h.waitStatus(func(s Status) bool { return s.State == StateDisabled })
}

// An unreadable setting is never treated as leave-it-running.
func TestSupervisorStopsOnUnreadableSetting(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.run()
	p := h.nextStart()
	h.mu.Lock()
	h.loadErr = errors.New("config.json: direct: unknown field \"listn\"")
	h.mu.Unlock()
	waitClosed(t, p.terminated, "terminate on invalid setting")
	st := h.waitStatus(func(s Status) bool { return s.State == StateInvalid })
	if !strings.Contains(st.LastError, "listn") {
		t.Fatalf("invalid status = %+v", st)
	}
}

func TestSupervisorStopsChildWhenDaemonStops(t *testing.T) {
	h := newHarness(t, enabledCfg)
	h.run()
	p := h.nextStart()
	h.cancel()
	<-h.done
	select {
	case <-p.terminated:
	default:
		t.Fatal("child not terminated on daemon stop")
	}
	st, err := ReadStatus(h.sup.StatusPath)
	if err != nil || st.State != StateStopped || st.PID != 0 {
		t.Fatalf("final status = %+v, %v", st, err)
	}
}
