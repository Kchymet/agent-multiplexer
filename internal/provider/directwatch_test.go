package provider

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// liveEnroller is a direct server whose answer changes under the provider: the
// daemon-managed listener starting, stopping, or moving.
type liveEnroller struct {
	mu       sync.Mutex
	caps     *hp.DirectCapabilities
	failNext int // fail this many upcoming probes (a listener restarting)
}

func (f *liveEnroller) set(c *hp.DirectCapabilities) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps = c
}

func (f *liveEnroller) Capabilities(context.Context) (*hp.DirectCapabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext > 0 {
		f.failNext--
		return nil, errors.New("dial admin.sock: connection refused")
	}
	return f.caps, nil
}

func (f *liveEnroller) Enroll(context.Context, string, hp.DirectEnrollRequest) hp.DirectEnrollResult {
	return hp.DirectEnrollResult{Error: hp.DirectEnrollErrUnavailable}
}

func fastRecheck(p *Provider) {
	p.directRecheck, p.directMisses = 10*time.Millisecond, 2
}

type watchRig struct {
	t     *testing.T
	p     *Provider
	f     *liveEnroller
	conns chan net.Conn
}

func startWatchRig(t *testing.T, initial *hp.DirectCapabilities, compute bool) *watchRig {
	t.Helper()
	r := &watchRig{t: t, f: &liveEnroller{caps: initial}, conns: make(chan net.Conn, 1)}
	r.p = newFast(Config{Orchestrator: "pipe", Token: "tok-1", Name: "laptop", AllowCompute: compute,
		Dial: pipeDialer(r.conns), DirectEnroll: r.f, OnStatus: func(Status) {}})
	r.p.graceScale = time.Second // panes must outlive a re-registration
	fastRecheck(r.p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.p.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// register reads the next connection's register frame and accepts it.
func (r *watchRig) register() (*hp.Conn, hp.HarnessMsg) {
	r.t.Helper()
	var c net.Conn
	select {
	case c = <-r.conns:
	case <-time.After(3 * time.Second):
		r.t.Fatal("provider did not (re)connect")
	}
	oc := hp.NewConn(c)
	reg := expectRegister(r.t, oc)
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MRegistered, OK: true, Version: 2, ProviderID: "prov-1",
		HeartbeatSeconds: 1, GraceSeconds: 60}); err != nil {
		r.t.Fatal(err)
	}
	r.waitStatus(func(s Status) bool { return s.State == StateRegistered })
	return oc, reg
}

func (r *watchRig) noReconnect(d time.Duration) {
	r.t.Helper()
	select {
	case <-r.conns:
		r.t.Fatal("provider re-registered without a real change")
	case <-time.After(d):
	}
}

func (r *watchRig) waitStatus(pred func(Status) bool) Status {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.p.stMu.Lock()
		st := r.p.status
		r.p.stMu.Unlock()
		if pred(st) {
			return st
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("status never matched: %+v", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func advertises(reg hp.HarnessMsg) bool {
	return reg.Capabilities != nil && reg.Capabilities.Direct != nil &&
		slices.Contains(reg.Capabilities.Features, hp.DirectEnrollV1)
}

// Enabling the managed listener after the provider registered gets it
// advertised without restarting the provider: same token, same name.
func TestDirectOfferAppearsWhenListenerStarts(t *testing.T) {
	r := startWatchRig(t, nil, false)
	_, reg := r.register()
	if advertises(reg) {
		t.Fatal("advertised direct enrollment with no listener")
	}
	if st := r.waitStatus(func(Status) bool { return true }); st.DirectServerID != "" {
		t.Fatalf("status claims an offer: %+v", st)
	}
	r.f.set(offered)
	_, reg2 := r.register()
	if !advertises(reg2) || reg2.Capabilities.Direct.ServerID != "sid" {
		t.Fatalf("re-registration caps %+v", reg2.Capabilities)
	}
	if reg2.Token != "tok-1" || reg2.Name != "laptop" {
		t.Fatalf("re-registration changed identity: token %q name %q", reg2.Token, reg2.Name)
	}
	r.waitStatus(func(s Status) bool {
		return s.DirectServerID == "sid" && slices.Equal(s.DirectEndpoints, offered.Endpoints)
	})
}

// Disabling the listener withdraws the offer: the provider re-registers
// without it and its status stops claiming one, while a running pane is
// offered for resume on the new registration (relay sessions survive).
func TestDirectOfferWithdrawnWhenListenerStops(t *testing.T) {
	r := startWatchRig(t, offered, true)
	oc, reg := r.register()
	if !advertises(reg) {
		t.Fatal("initial registration did not advertise")
	}
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MSpawn, PaneID: "p1", Argv: []string{"sh", "-c", "sleep 5"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.p.paneCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	r.f.set(nil)
	_, reg2 := r.register()
	if advertises(reg2) {
		t.Fatalf("still advertised after the listener stopped: %+v", reg2.Capabilities.Direct)
	}
	if len(reg2.Panes) != 1 || reg2.Panes[0].PaneID != "p1" || !reg2.Panes[0].Running {
		t.Fatalf("pane not carried across the re-registration: %+v", reg2.Panes)
	}
	st := r.waitStatus(func(s Status) bool { return s.State == StateRegistered })
	if st.DirectServerID != "" || st.DirectEndpoints != nil {
		t.Fatalf("status still claims the withdrawn offer: %+v", st)
	}
}

func TestDirectOfferFollowsEndpointChange(t *testing.T) {
	r := startWatchRig(t, offered, false)
	r.register()
	moved := &hp.DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://moved.test:8443"}, Issuance: true}
	r.f.set(moved)
	_, reg := r.register()
	if !advertises(reg) || !slices.Equal(reg.Capabilities.Direct.Endpoints, moved.Endpoints) {
		t.Fatalf("re-registration caps %+v", reg.Capabilities.Direct)
	}
}

// One failed probe is a listener restarting (a renewed certificate, a
// supervisor restart), not a withdrawal: the provider keeps its registration.
func TestDirectOfferSurvivesTransientProbeFailure(t *testing.T) {
	r := startWatchRig(t, offered, false)
	r.register()
	r.f.mu.Lock()
	r.f.failNext = 1
	r.f.mu.Unlock()
	r.noReconnect(150 * time.Millisecond)
	// A sustained failure is real, and withdraws the offer.
	r.f.mu.Lock()
	r.f.failNext = 1 << 30
	r.f.mu.Unlock()
	_, reg := r.register()
	if advertises(reg) {
		t.Fatal("advertised while the direct server was unreachable")
	}
}

// No direct enroller configured: nothing is watched and nothing changes.
func TestNoDirectWatchWithoutEnroller(t *testing.T) {
	conns := make(chan net.Conn, 1)
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns)})
	fastRecheck(p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	accept(t, hp.NewConn(<-conns), 2, nil, 60)
	select {
	case <-conns:
		t.Fatal("re-registered without a direct enroller")
	case <-time.After(100 * time.Millisecond):
	}
}
