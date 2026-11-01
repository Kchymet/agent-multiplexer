package provider

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

type fakeEnroller struct {
	mu      sync.Mutex
	caps    *hp.DirectCapabilities
	capsErr error
	calls   []string // provider ids passed to Enroll
	reqs    []hp.DirectEnrollRequest
	result  hp.DirectEnrollResult
	block   chan struct{}
	entered chan struct{}
}

func (f *fakeEnroller) Capabilities(context.Context) (*hp.DirectCapabilities, error) {
	return f.caps, f.capsErr
}

func (f *fakeEnroller) Enroll(ctx context.Context, providerID string, req hp.DirectEnrollRequest) hp.DirectEnrollResult {
	f.mu.Lock()
	f.calls = append(f.calls, providerID)
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return f.result
}

var offered = &hp.DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://amux.test:8443"}, Issuance: true}

func goodEnroll() *hp.DirectEnrollRequest {
	return &hp.DirectEnrollRequest{IssueKey: "k1", Requester: "u1", CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGEgnrNKBk",
		CodeChallengeMethod: hp.DirectEnrollMethodS256, Permissions: []string{"read"},
		Resource: hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"wg1"}}}
}

func registeredProvider(t *testing.T, f *fakeEnroller) *Provider {
	t.Helper()
	p := New(Config{DirectEnroll: f})
	p.discoverDirect(t.Context())
	return p
}

func TestDirectEnrollAdvertisedOnlyWhenIssuanceEnabled(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  Config
		want bool
	}{
		"no enroller":             {Config{Features: []string{hp.DirectEnrollV1}}, false},
		"issuance off":            {Config{DirectEnroll: &fakeEnroller{caps: nil}}, false},
		"issuance false":          {Config{DirectEnroll: &fakeEnroller{caps: &hp.DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://a"}}}}, false},
		"status error":            {Config{DirectEnroll: &fakeEnroller{caps: offered, capsErr: context.DeadlineExceeded}}, false},
		"enabled":                 {Config{DirectEnroll: &fakeEnroller{caps: offered}}, true},
		"enabled, feature listed": {Config{DirectEnroll: &fakeEnroller{caps: offered}, Features: []string{hp.DirectEnrollV1}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			p := New(tc.cfg)
			p.discoverDirect(t.Context())
			caps := p.capabilities()
			has := slices.Contains(caps.Features, hp.DirectEnrollV1)
			if has != tc.want || (caps.Direct != nil) != tc.want {
				t.Fatalf("advertised=%v direct=%+v, want %v", has, caps.Direct, tc.want)
			}
			if n := len(slices.DeleteFunc(slices.Clone(caps.Features), func(f string) bool { return f != hp.DirectEnrollV1 })); n > 1 {
				t.Fatal("duplicate feature")
			}
		})
	}
}

func readEnrollReply(t *testing.T, right net.Conn) hp.HarnessMsg {
	t.Helper()
	_ = right.SetReadDeadline(time.Now().Add(5 * time.Second))
	m, err := hp.NewConn(right).ReadHarness()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != hp.HDirectEnrollResult || m.DirectEnroll == nil {
		t.Fatalf("reply %+v", m)
	}
	return m
}

func TestDirectEnrollUsesRegisteredProviderID(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := &fakeEnroller{caps: offered, result: hp.DirectEnrollResult{Bundle: "amux-direct:1:x", ExpiresAt: 1, ServerID: "sid"}}
	p := registeredProvider(t, f)
	s := &session{hc: hp.NewConn(left), rtCtx: t.Context(), providerID: "prov-registered"}
	// A frame cannot name the provider: there is no field for it, and stray
	// session-action fields are ignored.
	p.handleDirectEnroll(s, hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "r1", ID: "prov-forged", DirectEnroll: goodEnroll()})
	m := readEnrollReply(t, right)
	if m.ReqID != "r1" || m.DirectEnroll.Bundle == "" || m.DirectEnroll.Error != "" {
		t.Fatalf("reply %+v", m.DirectEnroll)
	}
	if len(f.calls) != 1 || f.calls[0] != "prov-registered" {
		t.Fatalf("provider ids %v", f.calls)
	}
}

func TestDirectEnrollRefusals(t *testing.T) {
	bad := goodEnroll()
	bad.IssueKey = ""
	long := goodEnroll()
	long.Requester = string(make([]byte, 200))
	for name, tc := range map[string]struct {
		enroller *fakeEnroller
		msg      hp.MuxMsg
		status   string
		want     string
	}{
		"not offered":        {&fakeEnroller{}, hp.MuxMsg{ReqID: "r", DirectEnroll: goodEnroll()}, "prov", hp.DirectEnrollErrDisabled},
		"no request":         {&fakeEnroller{caps: offered}, hp.MuxMsg{ReqID: "r"}, "prov", hp.DirectEnrollErrInvalidRequest},
		"empty issue key":    {&fakeEnroller{caps: offered}, hp.MuxMsg{ReqID: "r", DirectEnroll: bad}, "prov", hp.DirectEnrollErrInvalidRequest},
		"oversized field":    {&fakeEnroller{caps: offered}, hp.MuxMsg{ReqID: "r", DirectEnroll: long}, "prov", hp.DirectEnrollErrInvalidRequest},
		"not registered yet": {&fakeEnroller{caps: offered}, hp.MuxMsg{ReqID: "r", DirectEnroll: goodEnroll()}, "", hp.DirectEnrollErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			p := New(Config{DirectEnroll: tc.enroller})
			p.discoverDirect(t.Context())
			s := &session{hc: hp.NewConn(left), rtCtx: t.Context(), providerID: tc.status}
			tc.msg.Type = hp.MDirectEnroll
			go p.handleDirectEnroll(s, tc.msg)
			m := readEnrollReply(t, right)
			if m.DirectEnroll.Error != tc.want || m.DirectEnroll.Bundle != "" {
				t.Fatalf("reply %+v", m.DirectEnroll)
			}
			if len(tc.enroller.calls) != 0 {
				t.Fatal("refused request reached the host")
			}
		})
	}
	// No ReqID: nothing to correlate, nothing sent, nothing called.
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := &fakeEnroller{caps: offered}
	p := registeredProvider(t, f)
	p.handleDirectEnroll(&session{hc: hp.NewConn(left), rtCtx: t.Context(), providerID: "prov-registered"}, hp.MuxMsg{Type: hp.MDirectEnroll, DirectEnroll: goodEnroll()})
	if len(f.calls) != 0 {
		t.Fatal("uncorrelated request reached the host")
	}
}

func TestDirectEnrollBoundedReplyAndConcurrency(t *testing.T) {
	old := directEnrollTimeout
	directEnrollTimeout = 100 * time.Millisecond
	t.Cleanup(func() { directEnrollTimeout = old })
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := &fakeEnroller{caps: offered, block: make(chan struct{}), entered: make(chan struct{}, 16), result: hp.DirectEnrollResult{Bundle: "late"}}
	defer close(f.block)
	p := registeredProvider(t, f)
	s := &session{hc: hp.NewConn(left), rtCtx: t.Context(), providerID: "prov-registered"}
	conn := hp.NewConn(right)
	for i := range maxDirectInFlight {
		p.handleDirectEnroll(s, hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: string(rune('a' + i)), DirectEnroll: goodEnroll()})
		<-f.entered
	}
	go p.handleDirectEnroll(s, hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "over", DirectEnroll: goodEnroll()})
	replies := map[string]string{}
	_ = right.SetReadDeadline(time.Now().Add(5 * time.Second))
	for range maxDirectInFlight + 1 {
		m, err := conn.ReadHarness()
		if err != nil {
			t.Fatal(err)
		}
		if m.DirectEnroll.Bundle != "" {
			t.Fatal("a timed-out request returned a bundle")
		}
		replies[m.ReqID] = m.DirectEnroll.Error
	}
	if replies["over"] != hp.DirectEnrollErrBusy {
		t.Fatalf("over-limit reply %q", replies["over"])
	}
	for i := range maxDirectInFlight {
		if r := replies[string(rune('a'+i))]; r != hp.DirectEnrollErrUnavailable {
			t.Fatalf("timed-out reply %q", r)
		}
	}
}

func TestDirectEnrollReadLoopDispatch(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := &fakeEnroller{caps: offered, result: hp.DirectEnrollResult{Error: hp.DirectEnrollErrOwnerMismatch}}
	p := registeredProvider(t, f)
	s := &session{hc: hp.NewConn(left), rtCtx: t.Context(), done: make(chan struct{}), providerID: "prov-registered"}
	go p.readLoop(s)
	conn := hp.NewConn(right)
	go func() { _ = conn.WriteMux(hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "rl", DirectEnroll: goodEnroll()}) }()
	m := readEnrollReply(t, right)
	if m.ReqID != "rl" || m.DirectEnroll.Error != hp.DirectEnrollErrOwnerMismatch {
		t.Fatalf("reply %+v", m)
	}
}

// Through the real register handshake: the advertisement rides on register and
// the provider id sent to the host is the one the orchestrator assigned.
func TestDirectEnrollOverRegisteredConnection(t *testing.T) {
	conns := make(chan net.Conn, 1)
	f := &fakeEnroller{caps: offered, result: hp.DirectEnrollResult{Bundle: "amux-direct:1:x", ServerID: "sid"}}
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns), DirectEnroll: f})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	oc := hp.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if !slices.Contains(reg.Capabilities.Features, hp.DirectEnrollV1) || reg.Capabilities.Direct == nil || reg.Capabilities.Direct.ServerID != "sid" {
		t.Fatalf("register caps %+v", reg.Capabilities)
	}
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MRegistered, OK: true, Version: 2, ProviderID: "prov-assigned", HeartbeatSeconds: 1, GraceSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "x1", DirectEnroll: goodEnroll()}); err != nil {
		t.Fatal(err)
	}
	for {
		m := readFrame(t, oc)
		if m.Type != hp.HDirectEnrollResult {
			continue
		}
		if m.ReqID != "x1" || m.DirectEnroll.Bundle == "" {
			t.Fatalf("reply %+v", m.DirectEnroll)
		}
		break
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 1 || f.calls[0] != "prov-assigned" {
		t.Fatalf("provider ids %v", f.calls)
	}
}
