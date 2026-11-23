package provider

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Every direct-enroll frame Harness relays to this provider is recorded in
// the status file and logged (time, kind, outcome, refusal code), never with
// its requester, issue key, challenge or bundle.
func TestDirectEnrollAttemptsRecordedAndLogged(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	var last Status
	f := &fakeEnroller{caps: offered, result: hp.DirectEnrollResult{Error: hp.DirectEnrollErrScope}}
	p := New(Config{DirectEnroll: f,
		OnStatus: func(s Status) { mu.Lock(); last = s; mu.Unlock() },
		Logf:     func(format string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(format, a...)); mu.Unlock() },
	})
	p.discoverDirect(t.Context())
	if p.status.StartedAt.IsZero() {
		t.Fatal("StartedAt not set")
	}
	send := func(req *hp.DirectEnrollRequest) hp.DirectEnrollResult {
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		go p.handleDirectEnroll(&session{hc: hp.NewConn(left), rtCtx: t.Context(), providerID: "prov"}, hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "r", DirectEnroll: req})
		return *readEnrollReply(t, right).DirectEnroll
	}

	if r := send(goodEnroll()); r.Error != hp.DirectEnrollErrScope {
		t.Fatalf("reply %+v", r)
	}
	mu.Lock()
	got := last.DirectEnrollLast
	mu.Unlock()
	if got == nil || got.Outcome != "refused" || got.Code != hp.DirectEnrollErrScope || got.Kind != "pkce_code" || got.At.IsZero() || last.DirectEnrollCount != 1 {
		t.Fatalf("recorded %+v (count %d)", got, last.DirectEnrollCount)
	}

	// A malformed frame refused by the provider itself still reached the host.
	if r := send(nil); r.Error != hp.DirectEnrollErrInvalidRequest {
		t.Fatalf("reply %+v", r)
	}
	mu.Lock()
	if last.DirectEnrollLast.Code != hp.DirectEnrollErrInvalidRequest || last.DirectEnrollLast.Kind != "other" || last.DirectEnrollCount != 2 {
		t.Fatalf("recorded %+v", last.DirectEnrollLast)
	}
	mu.Unlock()

	f.mu.Lock()
	f.result = hp.DirectEnrollResult{Bundle: "amux-direct:2:placeholder", ExpiresAt: 1, ServerID: "sid"}
	f.mu.Unlock()
	ticket := goodEnroll()
	ticket.Handoff, ticket.CodeChallenge, ticket.CodeChallengeMethod = hp.DirectHandoffDeviceTicket, "", ""
	if r := send(ticket); r.Bundle == "" {
		t.Fatalf("reply %+v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if l := last.DirectEnrollLast; l.Outcome != "issued" || l.Code != "" || l.Kind != "device_ticket" || last.DirectEnrollCount != 3 {
		t.Fatalf("recorded %+v", l)
	}
	raw, _ := json.Marshal(last)
	all := strings.Join(logs, "\n") + string(raw)
	for _, secret := range []string{"amux-direct:2:placeholder", goodEnroll().CodeChallenge, `"k1"`, "u1 "} {
		if strings.Contains(all, secret) {
			t.Fatalf("status or log leaks %q:\n%s", secret, all)
		}
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "direct-enroll request from Harness reached this host") {
			n++
		}
	}
	if n != 3 || !strings.Contains(strings.Join(logs, "\n"), "refused: scope_exceeds_ceiling (pkce_code)") {
		t.Fatalf("logs %q", logs)
	}
}

// A request naming no permissions is passed through: the host applies its
// default (read, plus steer when its ceiling allows it).
func TestDirectEnrollNoPermissionsReachesHost(t *testing.T) {
	r := goodEnroll()
	r.Permissions = nil
	if !validDirectRequest(r) {
		t.Fatal("a request with no permissions was refused by the provider")
	}
	r.Permissions = make([]string, maxDirectPerms+1)
	if validDirectRequest(r) {
		t.Fatal("oversized permissions accepted")
	}
}
