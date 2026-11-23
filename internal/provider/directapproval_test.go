package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Placeholder approval with the right shape; obviously not real.
var testApproval = hp.DirectApprovalPrefix + strings.Repeat("A", 43)

type fakeApprover struct {
	mu      sync.Mutex
	items   chan []ApprovalItem
	settled []ApprovalVerdict
	got     chan ApprovalVerdict
}

func newFakeApprover() *fakeApprover {
	return &fakeApprover{items: make(chan []ApprovalItem, 4), got: make(chan ApprovalVerdict, 8)}
}

func (f *fakeApprover) LeaseApprovals(ctx context.Context, max int, wait time.Duration) ([]ApprovalItem, error) {
	select {
	case it := <-f.items:
		return it, nil
	case <-time.After(20 * time.Millisecond):
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeApprover) SettleApprovals(ctx context.Context, verdicts []ApprovalVerdict) error {
	f.mu.Lock()
	f.settled = append(f.settled, verdicts...)
	f.mu.Unlock()
	for _, v := range verdicts {
		f.got <- v
	}
	return nil
}

var approvalOffer = &hp.DirectCapabilities{ServerID: "sid", Endpoints: []string{"https://amux.test:8443"}, Issuance: true, Handoff: true,
	Approval: true, Ceiling: &hp.DirectCeiling{Permissions: []string{"read", "steer"}, Resource: hp.DirectResourceAllSessions}}

// A registered provider whose direct server serves approvals advertises
// direct-approval-v1 with approval and the ceiling, records webOrigin, sends
// one direct-approval-verify per parked verify and settles Harness's answer
// with its registered provider id. The approval is never logged.
func TestDirectApprovalVerifyOverRegisteredConnection(t *testing.T) {
	conns := make(chan net.Conn, 1)
	ap := newFakeApprover()
	var mu sync.Mutex
	var logs []string
	var last Status
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns), DirectEnroll: &fakeEnroller{caps: approvalOffer}, DirectApprove: ap,
		OnStatus: func(s Status) { mu.Lock(); last = s; mu.Unlock() },
		Logf:     func(f string, v ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, v...)); mu.Unlock() }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()
	oc := hp.NewConn(<-conns)
	reg := expectRegister(t, oc)
	caps := reg.Capabilities
	if !slices.Contains(caps.Features, hp.DirectApprovalV1) || !slices.Contains(caps.Features, hp.DirectEnrollV1) || caps.Direct == nil || !caps.Direct.Approval {
		t.Fatalf("register caps %+v direct %+v", caps.Features, caps.Direct)
	}
	raw, _ := json.Marshal(caps.Direct)
	if !strings.Contains(string(raw), `"approval":true,"ceiling":{"permissions":["read","steer"],"resource":"all_sessions"}`) {
		t.Fatalf("direct offer wire %s", raw)
	}
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MRegistered, OK: true, Version: 2, ProviderID: "prov-assigned", HeartbeatSeconds: 1, GraceSeconds: 1,
		WebOrigin: "https://harness.example.dev"}); err != nil {
		t.Fatal(err)
	}
	ap.items <- []ApprovalItem{{ID: "q1", Verify: hp.DirectApprovalVerify{Approval: testApproval, ServerID: "sid", CodeChallenge: "ch"}}}
	var verify hp.HarnessMsg
	for {
		m := readFrame(t, oc)
		if m.Type == hp.HDirectApprovalVerify {
			verify = m
			break
		}
	}
	if verify.ReqID == "" || verify.DirectApproval == nil || *verify.DirectApproval != (hp.DirectApprovalVerify{Approval: testApproval, ServerID: "sid", CodeChallenge: "ch"}) {
		t.Fatalf("verify frame %+v", verify)
	}
	ok := hp.DirectApprovalResult{Requester: "owner", Permissions: []string{"read"}, Resource: &hp.DirectResource{Kind: hp.DirectResourceAllSessions}, ExpiresAt: 99}
	if err := oc.WriteMux(hp.MuxMsg{Type: hp.MDirectApprovalResult, ReqID: verify.ReqID, DirectApprovalResult: &ok}); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-ap.got:
		if v.ID != "q1" || v.ProviderID != "prov-assigned" || v.Result.Requester != "owner" || v.Result.Error != "" {
			t.Fatalf("settled %+v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("never settled")
	}
	mu.Lock()
	defer mu.Unlock()
	if last.WebOrigin != "https://harness.example.dev" {
		t.Fatalf("status webOrigin %q", last.WebOrigin)
	}
	joined := strings.Join(logs, "\n")
	if strings.Contains(joined, testApproval) || !strings.Contains(joined, "direct-approval verify: ok") {
		t.Fatalf("logs:\n%s", joined)
	}
}

// No answer within the bound, a malformed verify, or more than four in flight:
// each is settled unavailable (or mismatch) promptly, never hangs.
func TestDirectApprovalBounds(t *testing.T) {
	old := directApprovalTimeout
	directApprovalTimeout = 100 * time.Millisecond
	t.Cleanup(func() { directApprovalTimeout = old })
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	go func() { // a silent orchestrator: reads and drops every frame
		c := hp.NewConn(right)
		for {
			if _, err := c.ReadHarness(); err != nil {
				return
			}
		}
	}()
	p := New(Config{DirectEnroll: &fakeEnroller{caps: approvalOffer}, DirectApprove: newFakeApprover()})
	s := &session{hc: hp.NewConn(left), rtCtx: t.Context(), done: make(chan struct{}), providerID: "prov"}
	v := hp.DirectApprovalVerify{Approval: testApproval, ServerID: "sid", CodeChallenge: "ch"}
	if r := p.verifyApproval(s, v); r.Error != hp.DirectApprovalErrUnavailable {
		t.Fatalf("timeout: %+v", r)
	}
	if r := p.verifyApproval(s, hp.DirectApprovalVerify{Approval: "nope", ServerID: "sid", CodeChallenge: "ch"}); r.Error != hp.DirectApprovalErrMismatch {
		t.Fatalf("malformed: %+v", r)
	}
	s.approval.mu.Lock()
	s.approval.inFlight = maxDirectApprovalsInFlight
	s.approval.mu.Unlock()
	start := time.Now()
	if r := p.verifyApproval(s, v); r.Error != hp.DirectApprovalErrUnavailable || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("over the in-flight bound: %+v", r)
	}
}

// Approval is advertised only with the verify bridge, and a changed ceiling
// or approval flag is an offer change (the provider re-registers).
func TestDirectApprovalOfferRules(t *testing.T) {
	p := New(Config{DirectEnroll: &fakeEnroller{caps: approvalOffer}})
	p.discoverDirect(t.Context())
	if d := p.directOffer(); d == nil || d.Approval || d.Ceiling != nil || slices.Contains(p.features(), hp.DirectApprovalV1) {
		t.Fatalf("offered approval without a bridge: %+v", d)
	}
	p = New(Config{DirectEnroll: &fakeEnroller{caps: approvalOffer}, DirectApprove: newFakeApprover(), Features: []string{hp.DirectApprovalV1}})
	p.discoverDirect(t.Context())
	if feats := p.features(); len(slices.DeleteFunc(slices.Clone(feats), func(f string) bool { return f != hp.DirectApprovalV1 })) != 1 {
		t.Fatalf("features %v", feats)
	}
	other := *approvalOffer
	if !sameDirectOffer(approvalOffer, &other) {
		t.Fatal("identical offers differ")
	}
	other.Ceiling = &hp.DirectCeiling{Permissions: []string{"read"}, Resource: hp.DirectResourceAllSessions}
	if sameDirectOffer(approvalOffer, &other) {
		t.Fatal("a ceiling change is not an offer change")
	}
	other = *approvalOffer
	other.Approval = false
	if sameDirectOffer(approvalOffer, &other) {
		t.Fatal("an approval change is not an offer change")
	}
}
