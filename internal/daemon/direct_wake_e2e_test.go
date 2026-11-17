package daemon_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/hostbackend"
	"amux/internal/provider"
	"amux/internal/store"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestDirectLiveWakeEndToEnd drives a content-free wake through the whole
// production path with the provider's content relay OFF ("sessions",
// "runtime-events" and "direct-enroll-v1" all disabled):
//
//	real daemon + structured codex runtime (fake App Server) raises a
//	permission -> direct server wake detector -> wake.json outbox -> the real
//	provider (direct-wake-v1, echoed) -> a fake orchestrator receives an
//	opaque direct-wake frame -> the phone fetches the details over direct with
//	its own grant, answers with the existing permission verb, sees the
//	resolution revision, and acks.
//
// The orchestrator and FCM are the fake boundary: the frame is checked to
// carry routing handles only.
func TestDirectLiveWakeEndToEnd(t *testing.T) {
	h := daemon.NewLiveHostForTest(t)
	root := t.TempDir()
	auth, err := direct.OpenAuthority(root, direct.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auth.Close)
	backend := direct.HostBackend{
		H:    hostbackend.WithDialer(func(ctx context.Context) (hostbackend.Conn, error) { return h.Dial(ctx) }),
		Dial: h.Dial,
		Probe: func(context.Context) []direct.CatalogHarness {
			return []direct.CatalogHarness{{Name: "claude"}, {Name: "codex"}}
		},
	}
	logf := func(f string, a ...any) { t.Logf("direct: "+f, a...) }
	wake := direct.NewWakeService(auth, root, backend, logf)
	srv, err := direct.NewServer(direct.Config{Authority: auth, Backend: backend, Endpoints: []string{"https://" + e2eHost}, Logf: logf, Wake: wake})
	if err != nil {
		t.Fatal(err)
	}
	e := &e2e{t: t, h: h, auth: auth, srv: srv}
	if _, err := auth.EnableWake("usr_owner", "prov-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go wake.Run(ctx)

	// A structured codex member whose runtime will ask for approval.
	all := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})
	code, rootWG, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"repos": ""}, "w-root")
	if code != 200 {
		t.Fatalf("create root: %d %+v", code, bad)
	}
	code, member, bad := e.act(all, rootWG.NewID, direct.VerbAddAgent, map[string]string{"agent": "codex"}, "w-agent")
	if code != 200 {
		t.Fatalf("add codex agent: %d %+v", code, bad)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	sess, _, _ := db.GetSession(member.NewID)
	db.Close()
	app := newApprovalAppServer(t)
	e.h.AttachStructured(t, member.NewID, sess.Dir, app.endpoint)

	// The phone subscribes with a route id from Harness and its own secret.
	route := hp.DirectWakeRoutePrefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	secret := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if code, b := e.call("POST", direct.PathWakeSubscribe, all, direct.WakeSubscribeRequest{RouteID: route, ActivationSecret: secret}); code != 200 {
		t.Fatalf("subscribe: %d %s", code, b)
	}

	// The real provider, content relay off, talking to a fake orchestrator.
	orch := newFakeOrchestrator(t)
	p := provider.New(provider.Config{
		Token: "tok", Orchestrator: "pipe", Dial: orch.dial,
		DirectWake: liveWaker{direct.NewWakeRelay(func(ctx context.Context, req direct.AdminRequest) (direct.AdminResponse, error) {
			return direct.AdministerWake(ctx, auth, wake, req), nil
		})},
		Logf: func(f string, a ...any) { t.Logf("provider: "+f, a...) },
	})
	go func() { _ = p.Run(ctx) }()
	orch.waitActivated(t, route, secret)

	// Raise the permission.
	if code, _, bad := e.act(all, member.NewID, direct.VerbPrompt, map[string]string{"text": "clean up"}, "w-prompt"); code != 200 {
		t.Fatalf("prompt: %d %+v", code, bad)
	}
	frame := orch.waitWake(t, route)
	if !hp.ValidDirectWakeFrame(hp.HDirectWake, frame) {
		t.Fatalf("wake frame shape: %+v", frame)
	}
	raw, _ := json.Marshal(frame)
	for _, leak := range []string{member.NewID, "rm -rf", "request_id", "runtime_generation", secret} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("wake frame leaked %q: %s", leak, raw)
		}
	}

	// The phone fetches the details over direct with its own grant.
	var item direct.WakeItem
	deadline := time.Now().Add(10 * time.Second)
	for item.RequestID == "" {
		if time.Now().After(deadline) {
			t.Fatal("no open permission in the wake fetch")
		}
		code, b := e.call("POST", direct.PathWakeEvents, all, direct.WakeFetchRequest{RouteID: route})
		var f direct.WakeFetchResponse
		if code == 200 && json.Unmarshal(b, &f) == nil {
			for _, it := range f.Items {
				if it.Kind == direct.WakeKindPermission && it.State == direct.WakeStateOpen {
					item = it
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if item.SessionID != member.NewID || item.RuntimeGeneration == "" || item.Event == nil {
		t.Fatalf("open permission item = %+v", item)
	}
	// Answer with the existing direct permission verb.
	if code, _, bad := e.act(all, member.NewID, direct.VerbPermission, map[string]string{
		"request_id": item.RequestID, "runtime_generation": item.RuntimeGeneration, "decision": "deny"}, "w-answer"); code != 200 {
		t.Fatalf("answer: %d %+v", code, bad)
	}
	select {
	case <-app.decisions:
	case <-time.After(5 * time.Second):
		t.Fatal("the decision never reached the runtime")
	}
	// The resolution arrives as a new revision of the same card.
	var resolved direct.WakeItem
	deadline = time.Now().Add(10 * time.Second)
	for resolved.WakeID == "" {
		if time.Now().After(deadline) {
			t.Fatal("no resolution revision")
		}
		code, b := e.call("POST", direct.PathWakeEvents, all, direct.WakeFetchRequest{RouteID: route, AfterSeq: item.Seq})
		var f direct.WakeFetchResponse
		if code == 200 && json.Unmarshal(b, &f) == nil {
			for _, it := range f.Items {
				if it.WakeID == item.WakeID && it.State == direct.WakeStateResolved {
					resolved = it
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if code, b := e.call("POST", direct.PathWakeAck, all, direct.WakeAckRequest{RouteID: route,
		Items: []direct.WakeAckItem{{WakeID: resolved.WakeID, Seq: resolved.Seq}}}); code != 200 || !strings.Contains(string(b), `"acked":1`) {
		t.Fatalf("ack: %d %s", code, b)
	}
}

// liveWaker adapts direct.WakeRelay to provider.DirectWaker (as cmd/amux does).
type liveWaker struct{ r *direct.WakeRelay }

func (w liveWaker) Status(ctx context.Context) (*provider.WakeOffer, error) {
	o, err := w.r.Status(ctx)
	if err != nil || o == nil {
		return nil, err
	}
	return &provider.WakeOffer{ServerID: o.ServerID, OwnerPin: o.OwnerPin, ProviderPin: o.ProviderPin}, nil
}

func (w liveWaker) Lease(ctx context.Context, max int, wait time.Duration) ([]provider.WakeItem, error) {
	items, err := w.r.Lease(ctx, max, wait)
	out := make([]provider.WakeItem, 0, len(items))
	for _, it := range items {
		out = append(out, provider.WakeItem{ID: it.ID, Kind: it.Kind, RouteID: it.RouteID, WakeID: it.WakeID, GrantRef: it.GrantRef, Requester: it.Requester, Secret: it.Secret})
	}
	return out, err
}

func (w liveWaker) Settle(ctx context.Context, rs []provider.WakeSettle) error {
	out := make([]direct.WakeSettleResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, direct.WakeSettleResult{ID: r.ID, Status: r.Status})
	}
	return w.r.Settle(ctx, out)
}

func (w liveWaker) Routes(ctx context.Context) ([]hp.DirectWakeRouteRef, error) {
	return w.r.Routes(ctx)
}

// fakeOrchestrator answers one provider connection: it echoes
// direct-wake-v1, accepts every activation and wake, and records them.
type fakeOrchestrator struct {
	mu        sync.Mutex
	activated map[string]string // route -> secret
	wakes     []*hp.DirectWake
	conns     chan net.Conn
}

func newFakeOrchestrator(t *testing.T) *fakeOrchestrator {
	o := &fakeOrchestrator{activated: map[string]string{}, conns: make(chan net.Conn, 4)}
	go func() {
		for server := range o.conns {
			go o.serve(t, server)
		}
	}()
	return o
}

func (o *fakeOrchestrator) dial(context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	o.conns <- server
	return client, nil
}

func (o *fakeOrchestrator) serve(t *testing.T, conn net.Conn) {
	pc := hp.NewConn(conn)
	reg, err := pc.ReadHarness()
	if err != nil || reg.Type != hp.HRegister {
		return
	}
	var features []string
	if reg.Capabilities != nil && reg.Capabilities.DirectWake != nil {
		features = []string{hp.DirectWakeV1}
	}
	if err := pc.WriteMux(hp.MuxMsg{Type: hp.MRegistered, OK: true, Version: 2, ProviderID: "prov-1", HeartbeatSeconds: 60,
		GraceSeconds: 60, Owner: &hp.RegisteredOwner{UserID: "usr_owner"}, Features: features}); err != nil {
		return
	}
	for {
		m, err := pc.ReadHarness()
		if err != nil {
			return
		}
		var res hp.DirectWakeResult
		switch m.Type {
		case hp.HPing:
			_ = pc.WriteMux(hp.MuxMsg{Type: hp.MPong, T: m.T})
			continue
		case hp.HDirectWakeActivate:
			o.mu.Lock()
			o.activated[m.DirectWake.RouteID] = m.DirectWake.ActivationSecret
			o.mu.Unlock()
			res.Results = []hp.DirectWakeResultItem{{RouteID: m.DirectWake.RouteID, Status: hp.DirectWakeAccepted}}
		case hp.HDirectWake:
			o.mu.Lock()
			o.wakes = append(o.wakes, m.DirectWake)
			o.mu.Unlock()
			for _, w := range m.DirectWake.Wakes {
				res.Results = append(res.Results, hp.DirectWakeResultItem{RouteID: w.RouteID, WakeID: w.WakeID, Status: hp.DirectWakeAccepted})
			}
		case hp.HDirectWakeRoutes:
		default:
			continue
		}
		_ = pc.WriteMux(hp.MuxMsg{Type: hp.MDirectWakeResult, ReqID: m.ReqID, DirectWakeResult: &res})
	}
}

func (o *fakeOrchestrator) waitActivated(t *testing.T, route, secret string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		got, ok := o.activated[route]
		o.mu.Unlock()
		if ok {
			if got != secret {
				t.Fatalf("activation carried another secret")
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the route was never activated")
}

func (o *fakeOrchestrator) waitWake(t *testing.T, route string) *hp.DirectWake {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		o.mu.Lock()
		for _, w := range o.wakes {
			for _, r := range w.Wakes {
				if r.RouteID == route {
					o.mu.Unlock()
					return w
				}
			}
		}
		o.mu.Unlock()
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no direct-wake frame reached the orchestrator")
	return nil
}
