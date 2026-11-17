package provider

import (
	"context"
	"encoding/base64"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

const testWakeServer = "AhW5GL1YTk5AXr4e0pvQx2pVoFpY6M3WC9uWFRvUw2U"

type fakeWaker struct {
	mu      sync.Mutex
	offer   *WakeOffer
	items   []WakeItem
	leased  bool
	settled []WakeSettle
	routes  []hp.DirectWakeRouteRef
	settleC chan struct{}
}

func (f *fakeWaker) Status(context.Context) (*WakeOffer, error) { return f.offer, nil }
func (f *fakeWaker) Lease(ctx context.Context, max int, wait time.Duration) ([]WakeItem, error) {
	f.mu.Lock()
	if !f.leased && len(f.items) > 0 {
		f.leased = true
		items := f.items
		f.mu.Unlock()
		return items, nil
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(20 * time.Millisecond):
	}
	return nil, nil
}
func (f *fakeWaker) Settle(_ context.Context, r []WakeSettle) error {
	f.mu.Lock()
	f.settled = append(f.settled, r...)
	f.mu.Unlock()
	if f.settleC != nil {
		f.settleC <- struct{}{}
	}
	return nil
}
func (f *fakeWaker) Routes(context.Context) ([]hp.DirectWakeRouteRef, error) { return f.routes, nil }

func wakeRoute(n byte) string {
	b := make([]byte, 32)
	b[0] = n
	return hp.DirectWakeRoutePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func wakeID(n byte) string {
	b := make([]byte, 16)
	b[0] = n
	return hp.DirectWakeIDPrefix + base64.RawURLEncoding.EncodeToString(b)
}

const wakeGrant = "b3BhcXVlLWdyYW50LXJlZg"

var wakeOffer = &WakeOffer{ServerID: testWakeServer, OwnerPin: "usr_1", ProviderPin: "prov-1"}

func TestDirectWakeAdvertisedOnlyWithOffer(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg    Config
		status Status
		want   bool
	}{
		"no waker":           {Config{Features: []string{hp.DirectWakeV1}}, Status{}, false},
		"wake off":           {Config{DirectWake: &fakeWaker{}}, Status{}, false},
		"enabled, first run": {Config{DirectWake: &fakeWaker{offer: wakeOffer}}, Status{}, true},
		"enabled, pins match": {Config{DirectWake: &fakeWaker{offer: wakeOffer}},
			Status{ProviderID: "prov-1", OwnerUserID: "usr_1"}, true},
		"enabled, other provider": {Config{DirectWake: &fakeWaker{offer: wakeOffer}},
			Status{ProviderID: "prov-2", OwnerUserID: "usr_1"}, false},
		"enabled, other owner": {Config{DirectWake: &fakeWaker{offer: wakeOffer}},
			Status{ProviderID: "prov-1", OwnerUserID: "usr_2"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			p := New(tc.cfg)
			p.status = tc.status
			p.discoverDirectWake(t.Context())
			caps := p.capabilities()
			has := slices.Contains(caps.Features, hp.DirectWakeV1)
			if has != tc.want || (caps.DirectWake != nil) != tc.want {
				t.Fatalf("advertised=%v caps=%+v, want %v", has, caps.DirectWake, tc.want)
			}
			if tc.want && caps.DirectWake.ServerID != testWakeServer {
				t.Fatalf("server id %q", caps.DirectWake.ServerID)
			}
		})
	}
}

// orchestrator reads provider frames off the pipe and answers each with the
// statuses chosen by answer.
func orchestrator(t *testing.T, right net.Conn, answer func(hp.HarnessMsg) *hp.DirectWakeResult) (frames chan hp.HarnessMsg) {
	t.Helper()
	frames = make(chan hp.HarnessMsg, 32)
	c := hp.NewConn(right)
	go func() {
		for {
			m, err := c.ReadHarness()
			if err != nil {
				close(frames)
				return
			}
			frames <- m
			if r := answer(m); r != nil {
				_ = c.WriteMux(hp.MuxMsg{Type: hp.MDirectWakeResult, ReqID: m.ReqID, DirectWakeResult: r})
			}
		}
	}()
	return frames
}

func wakeSession(t *testing.T, left net.Conn) *session {
	return &session{hc: hp.NewConn(left), rtCtx: t.Context(), done: make(chan struct{}), providerID: "prov-1"}
}

func TestForwardWakesActivationFirstFramesAndStatuses(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	p := New(Config{DirectWake: &fakeWaker{offer: wakeOffer}})
	s := wakeSession(t, left)
	go func() {
		c := hp.NewConn(left)
		_ = c
	}()
	frames := orchestrator(t, right, func(m hp.HarnessMsg) *hp.DirectWakeResult {
		switch m.Type {
		case hp.HDirectWakeActivate:
			return &hp.DirectWakeResult{Results: []hp.DirectWakeResultItem{{RouteID: m.DirectWake.RouteID, Status: hp.DirectWakeAccepted}}}
		case hp.HDirectWake:
			var rs []hp.DirectWakeResultItem
			for i, w := range m.DirectWake.Wakes {
				st := hp.DirectWakeAccepted
				if i == 1 {
					st = hp.DirectWakeRouteGone
				}
				rs = append(rs, hp.DirectWakeResultItem{RouteID: w.RouteID, WakeID: w.WakeID, Status: st})
			}
			return &hp.DirectWakeResult{Results: rs}
		}
		return nil
	})
	// The read loop is what delivers results in production; drive it here.
	go func() {
		c := s.hc
		for {
			m, err := c.ReadMux()
			if err != nil {
				return
			}
			if m.Type == hp.MDirectWakeResult {
				p.onDirectWakeResult(s, m)
			}
		}
	}()
	items := []WakeItem{
		{ID: "a", Kind: "activate", RouteID: wakeRoute(1), GrantRef: wakeGrant, Secret: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", Requester: "usr_1"},
		{ID: "w1", Kind: "wake", RouteID: wakeRoute(2), WakeID: wakeID(1), GrantRef: wakeGrant},
		{ID: "w2", Kind: "wake", RouteID: wakeRoute(3), WakeID: wakeID(2), GrantRef: wakeGrant},
	}
	got := collectSettles(p, s, items)
	want := map[string]string{"a": hp.DirectWakeAccepted, "w1": hp.DirectWakeAccepted, "w2": hp.DirectWakeRouteGone}
	for _, r := range got {
		if want[r.ID] != r.Status {
			t.Fatalf("settle %v, want %v", got, want)
		}
	}
	act, wake := <-frames, <-frames
	if act.Type != hp.HDirectWakeActivate {
		act, wake = wake, act // sent concurrently: order is not fixed
	}
	if act.Type != hp.HDirectWakeActivate || act.DirectWake.ActivationSecret == "" || !hp.ValidDirectWakeFrame(act.Type, act.DirectWake) {
		t.Fatalf("activate frame %+v", act.DirectWake)
	}
	if wake.Type != hp.HDirectWake || len(wake.DirectWake.Wakes) != 2 || wake.DirectWake.ActivationSecret != "" || !hp.ValidDirectWakeFrame(wake.Type, wake.DirectWake) {
		t.Fatalf("wake frame %+v", wake.DirectWake)
	}
}

func TestForwardWakesFrameFailureIsRetryable(t *testing.T) {
	saved := directWakeFrameTimeout
	directWakeFrameTimeout = 50 * time.Millisecond
	defer func() { directWakeFrameTimeout = saved }()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	p := New(Config{DirectWake: &fakeWaker{offer: wakeOffer}})
	s := wakeSession(t, left)
	_ = orchestrator(t, right, func(m hp.HarnessMsg) *hp.DirectWakeResult { return nil }) // never answers
	got := collectSettles(p, s, []WakeItem{{ID: "w", Kind: "wake", RouteID: wakeRoute(1), WakeID: wakeID(1), GrantRef: wakeGrant}})
	if len(got) != 1 || got[0].Status != hp.DirectWakeUnavailable {
		t.Fatalf("timeout settle = %+v", got)
	}
	// A frame-level refusal is retryable too.
	left2, right2 := net.Pipe()
	defer left2.Close()
	defer right2.Close()
	s2 := wakeSession(t, left2)
	_ = orchestrator(t, right2, func(m hp.HarnessMsg) *hp.DirectWakeResult { return &hp.DirectWakeResult{Error: hp.DirectWakeBusy} })
	go func() {
		for {
			m, err := s2.hc.ReadMux()
			if err != nil {
				return
			}
			p.onDirectWakeResult(s2, m)
		}
	}()
	got = collectSettles(p, s2, []WakeItem{{ID: "w", Kind: "wake", RouteID: wakeRoute(1), WakeID: wakeID(1), GrantRef: wakeGrant}})
	if got[0].Status != hp.DirectWakeUnavailable {
		t.Fatalf("frame error settle = %+v", got)
	}
}

func TestSyncRoutesSendsOneCompleteEpoch(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var routes []hp.DirectWakeRouteRef
	for i := 0; i < hp.MaxDirectWakeRoutes+10; i++ {
		b := make([]byte, 32)
		b[0], b[1] = byte(i), byte(i>>8)
		routes = append(routes, hp.DirectWakeRouteRef{RouteID: hp.DirectWakeRoutePrefix + base64.RawURLEncoding.EncodeToString(b), GrantRef: wakeGrant, State: hp.DirectWakeStateActive})
	}
	p := New(Config{DirectWake: &fakeWaker{offer: wakeOffer, routes: routes}})
	s := wakeSession(t, left)
	frames := orchestrator(t, right, func(m hp.HarnessMsg) *hp.DirectWakeResult { return &hp.DirectWakeResult{} })
	go func() {
		for {
			m, err := s.hc.ReadMux()
			if err != nil {
				return
			}
			p.onDirectWakeResult(s, m)
		}
	}()
	p.syncRoutes(s, wakeOffer)
	a, b := <-frames, <-frames
	if a.DirectWake.Epoch == "" || a.DirectWake.Epoch != b.DirectWake.Epoch || a.DirectWake.Part != 0 || b.DirectWake.Part != 1 ||
		a.DirectWake.Final || !b.DirectWake.Final || len(a.DirectWake.Routes)+len(b.DirectWake.Routes) != len(routes) {
		t.Fatalf("epoch parts: %+v / %+v", a.DirectWake.Epoch, b.DirectWake.Epoch)
	}
	for _, f := range []hp.HarnessMsg{a, b} {
		if !hp.ValidDirectWakeFrame(f.Type, f.DirectWake) {
			t.Fatalf("invalid routes frame part %d", f.DirectWake.Part)
		}
	}
}

// End to end over a pipe: the provider forwards only after the orchestrator
// echoes the feature, and settles with the orchestrator's answer.
func TestDirectWakeRunsOnlyAfterEcho(t *testing.T) {
	for _, echo := range []bool{false, true} {
		t.Run(map[bool]string{false: "not echoed", true: "echoed"}[echo], func(t *testing.T) {
			left, right := net.Pipe()
			fw := &fakeWaker{offer: wakeOffer, settleC: make(chan struct{}, 4),
				items: []WakeItem{{ID: "w", Kind: "wake", RouteID: wakeRoute(1), WakeID: wakeID(1), GrantRef: wakeGrant}}}
			dialed := false
			p := New(Config{DirectWake: fw, Token: "t", Dial: func(context.Context) (net.Conn, error) {
				if dialed {
					return nil, context.Canceled
				}
				dialed = true
				return left, nil
			}})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			go func() { _ = p.Run(ctx) }()
			c := hp.NewConn(right)
			reg, err := c.ReadHarness()
			if err != nil || reg.Type != hp.HRegister || reg.Capabilities.DirectWake == nil {
				t.Fatalf("register %+v %v", reg, err)
			}
			rm := hp.MuxMsg{Type: hp.MRegistered, OK: true, Version: 2, ProviderID: "prov-1", HeartbeatSeconds: 60,
				Owner: &hp.RegisteredOwner{UserID: "usr_1"}}
			if echo {
				rm.Features = []string{hp.DirectWakeV1}
			}
			_ = c.WriteMux(rm)
			got := make(chan hp.HarnessMsg, 8)
			go func() {
				for {
					m, err := c.ReadHarness()
					if err != nil {
						return
					}
					if m.Type == hp.HDirectWake || m.Type == hp.HDirectWakeRoutes || m.Type == hp.HDirectWakeActivate {
						got <- m
						var rs []hp.DirectWakeResultItem
						for _, w := range m.DirectWake.Wakes {
							rs = append(rs, hp.DirectWakeResultItem{RouteID: w.RouteID, WakeID: w.WakeID, Status: hp.DirectWakeAccepted})
						}
						_ = c.WriteMux(hp.MuxMsg{Type: hp.MDirectWakeResult, ReqID: m.ReqID, DirectWakeResult: &hp.DirectWakeResult{Results: rs}})
					}
				}
			}()
			if !echo {
				select {
				case m := <-got:
					t.Fatalf("frame sent without echo: %+v", m)
				case <-time.After(300 * time.Millisecond):
				}
				return
			}
			deadline := time.After(2 * time.Second)
			sawWake := false
			for !sawWake {
				select {
				case m := <-got:
					sawWake = m.Type == hp.HDirectWake
				case <-deadline:
					t.Fatal("no wake frame after echo")
				}
			}
			select {
			case <-fw.settleC:
			case <-time.After(2 * time.Second):
				t.Fatal("not settled")
			}
			fw.mu.Lock()
			defer fw.mu.Unlock()
			if len(fw.settled) != 1 || fw.settled[0].Status != hp.DirectWakeAccepted {
				t.Fatalf("settled %+v", fw.settled)
			}
		})
	}
}

func collectSettles(p *Provider, s *session, items []WakeItem) []WakeSettle {
	var mu sync.Mutex
	var out []WakeSettle
	p.forwardWakes(s, wakeOffer, items, func(r []WakeSettle) {
		mu.Lock()
		out = append(out, r...)
		mu.Unlock()
	})
	return out
}

// Several slow activations: an early answer is settled at once (while the
// others are still in flight), a slow one past the frame timeout stays
// retryable, and a wake in the same round is not held behind them.
func TestForwardWakesSettlesEachAnswerPromptly(t *testing.T) {
	saved := directWakeFrameTimeout
	directWakeFrameTimeout = 400 * time.Millisecond
	defer func() { directWakeFrameTimeout = saved }()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	p := New(Config{DirectWake: &fakeWaker{offer: wakeOffer}})
	s := wakeSession(t, left)
	c := hp.NewConn(right)
	go func() {
		for {
			m, err := c.ReadHarness()
			if err != nil {
				return
			}
			go func(m hp.HarnessMsg) {
				var res hp.DirectWakeResult
				switch {
				case m.Type == hp.HDirectWake:
					for _, w := range m.DirectWake.Wakes {
						res.Results = append(res.Results, hp.DirectWakeResultItem{RouteID: w.RouteID, WakeID: w.WakeID, Status: hp.DirectWakeAccepted})
					}
				case m.DirectWake.RouteID == wakeRoute(1): // fast
					res.Results = []hp.DirectWakeResultItem{{RouteID: wakeRoute(1), Status: hp.DirectWakeAccepted}}
				case m.DirectWake.RouteID == wakeRoute(2): // slow, still in time
					time.Sleep(200 * time.Millisecond)
					res.Results = []hp.DirectWakeResultItem{{RouteID: wakeRoute(2), Status: hp.DirectWakeAccepted}}
				default: // never answers in time
					return
				}
				_ = c.WriteMux(hp.MuxMsg{Type: hp.MDirectWakeResult, ReqID: m.ReqID, DirectWakeResult: &res})
			}(m)
		}
	}()
	go func() {
		for {
			m, err := s.hc.ReadMux()
			if err != nil {
				return
			}
			p.onDirectWakeResult(s, m)
		}
	}()
	type stamped struct {
		WakeSettle
		at time.Duration
	}
	var mu sync.Mutex
	var got []stamped
	start := time.Now()
	items := []WakeItem{
		{ID: "fast", Kind: "activate", RouteID: wakeRoute(1), GrantRef: wakeGrant, Secret: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{ID: "slow", Kind: "activate", RouteID: wakeRoute(2), GrantRef: wakeGrant, Secret: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{ID: "stuck", Kind: "activate", RouteID: wakeRoute(3), GrantRef: wakeGrant, Secret: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
	}
	p.forwardWakes(s, wakeOffer, items, func(r []WakeSettle) {
		mu.Lock()
		for _, x := range r {
			got = append(got, stamped{x, time.Since(start)})
		}
		mu.Unlock()
	})
	by := map[string]stamped{}
	for _, g := range got {
		by[g.ID] = g
	}
	if by["fast"].Status != hp.DirectWakeAccepted || by["fast"].at > 150*time.Millisecond {
		t.Fatalf("fast activation not settled promptly: %+v", by["fast"])
	}
	if by["slow"].Status != hp.DirectWakeAccepted {
		t.Fatalf("slow activation = %+v", by["slow"])
	}
	if by["stuck"].Status != hp.DirectWakeUnavailable {
		t.Fatalf("timed-out activation must stay retryable: %+v", by["stuck"])
	}
	if by["fast"].at >= by["slow"].at {
		t.Fatal("early success waited for a slower frame")
	}
	// A wake in a round with slow activations is answered without waiting.
	start = time.Now()
	got = nil
	p.forwardWakes(s, wakeOffer, []WakeItem{
		{ID: "stuck2", Kind: "activate", RouteID: wakeRoute(4), GrantRef: wakeGrant, Secret: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"},
		{ID: "w", Kind: "wake", RouteID: wakeRoute(5), WakeID: wakeID(5), GrantRef: wakeGrant},
	}, func(r []WakeSettle) {
		mu.Lock()
		for _, x := range r {
			got = append(got, stamped{x, time.Since(start)})
		}
		mu.Unlock()
	})
	for _, g := range got {
		if g.ID == "w" && (g.Status != hp.DirectWakeAccepted || g.at > 150*time.Millisecond) {
			t.Fatalf("wake held behind a slow activation: %+v", g)
		}
	}
	if directWakeLeaseMax >= maxDirectWakeInFlight {
		t.Fatal("a lease round must leave a frame slot for route sync")
	}
}
