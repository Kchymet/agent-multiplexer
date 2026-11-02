package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/hostbackend"
)

// Asynchronous creation against a real daemon. slowConn performs the real
// daemon dispatch (the daemon fetches, creates the worktree, writes config and
// starts the fake runtime) and then holds the answer until the test releases
// it: from the direct server's side the creation is as slow as a large repo
// fetch, while the daemon's effect is real and observable.

type slowConn struct {
	hostbackend.Conn
	gate       <-chan struct{}
	dispatches *atomic.Int64
	created    chan<- string
	closed     chan struct{}
	once       sync.Once
}

// Close ends a held Dispatch the way closing a real socket ends a blocked
// read: hostbackend.Join closes the transport on cancellation.
func (c *slowConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *slowConn) Dispatch(a core.Action) (string, error) {
	id, err := c.Conn.Dispatch(a)
	if !core.DescriptorFor(a.Action).CreatesSession {
		return id, err
	}
	c.dispatches.Add(1)
	select {
	case c.created <- id:
	default:
	}
	select {
	case <-c.gate:
		return id, err
	case <-c.closed:
		return "", errors.New("use of closed connection")
	}
}

type slowE2E struct {
	*e2e
	gate       chan struct{}
	dispatches atomic.Int64
	created    chan string
}

func newSlowE2E(t *testing.T) *slowE2E {
	t.Helper()
	h := daemon.NewLiveHostForTest(t)
	auth, err := direct.OpenAuthority(t.TempDir(), direct.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auth.Close)
	s := &slowE2E{gate: make(chan struct{}), created: make(chan string, 8)}
	backend := direct.HostBackend{
		H: hostbackend.WithDialer(func(ctx context.Context) (hostbackend.Conn, error) {
			c, err := h.Dial(ctx)
			if err != nil {
				return nil, err
			}
			return &slowConn{Conn: c, gate: s.gate, dispatches: &s.dispatches, created: s.created, closed: make(chan struct{})}, nil
		}),
		Dial: h.Dial,
		Probe: func(context.Context) []direct.CatalogHarness {
			return []direct.CatalogHarness{{Name: "claude"}, {Name: "codex"}}
		},
	}
	srv, err := direct.NewServer(direct.Config{Authority: auth, Backend: backend, Endpoints: []string{"https://" + e2eHost},
		Logf: func(f string, a ...any) { t.Logf("direct: "+f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	s.e2e = &e2e{t: t, h: h, auth: auth, srv: srv}
	return s
}

func (e *e2e) createAsync(tok, sid, verb string, fields map[string]string, key string) (int, direct.CreationStatus) {
	e.t.Helper()
	path := direct.PathWorkgroups
	if sid != "" {
		path = direct.PathSession + "/" + sid + "/actions"
	}
	code, b := e.call("POST", path, tok, direct.ActionRequest{Verb: verb, Fields: fields, IdemKey: key, Async: true})
	var st direct.CreationStatus
	_ = json.Unmarshal(b, &st)
	return code, st
}

func (e *e2e) creation(tok, id, query string) (int, direct.CreationStatus, direct.APIError) {
	e.t.Helper()
	path := direct.PathCreations + "/" + id
	if query != "" {
		path += "?" + query
	}
	code, b := e.call("GET", path, tok, nil)
	var st direct.CreationStatus
	var bad direct.APIError
	_ = json.Unmarshal(b, &st)
	_ = json.Unmarshal(b, &bad)
	return code, st, bad
}

func (e *e2e) awaitCreation(tok, id string) direct.CreationStatus {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	after := 0
	for time.Now().Before(deadline) {
		code, st, bad := e.creation(tok, id, fmt.Sprintf("wait=5&after=%d", after))
		if code != 200 {
			e.t.Fatalf("status: %d %+v", code, bad)
		}
		if st.Version >= 3 {
			return st
		}
		after = st.Version
	}
	e.t.Fatal("creation never finished")
	return direct.CreationStatus{}
}

// A slow real creation is accepted at once, reported preparing while the
// daemon works, and followed to ready with the daemon's own ids; replay and
// add-agent follow-ups route through the result. Exactly one daemon creation
// per key.
func TestDirectLiveAsyncSlowCreation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to track a repository")
	}
	e := newSlowE2E(t)
	trackLocalRepo(t, "app")
	all := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})
	fields := map[string]string{"name": "Slow", "prompt": "fix it", "repos": "app", "agent": "claude"}

	code, st := e.createAsync(all, "", direct.VerbNewWorkgroup, fields, "slow-1")
	if code != 202 || st.Phase != direct.CreatePhasePreparing || st.NewID != "" {
		t.Fatalf("accepted: %d %+v", code, st)
	}
	// The daemon really created it while the direct answer is still pending.
	root := <-e.created
	e.eventually(all, "daemon-created root", func(inv map[string]direct.Session) bool { return inv[root].IsRoot })
	if _, now, _ := e.creation(all, st.CreationID, ""); now.Phase != direct.CreatePhasePreparing {
		t.Fatalf("still preparing: %+v", now)
	}
	if code, again := e.createAsync(all, "", direct.VerbNewWorkgroup, fields, "slow-1"); code != 202 || again.CreationID != st.CreationID {
		t.Fatalf("re-POST in flight: %d %+v", code, again)
	}
	if code, _, bad := e.act(all, "", direct.VerbNewWorkgroup, fields, "slow-1"); code != 409 || bad.Code != direct.CodeIdemInProgress {
		t.Fatalf("sync in flight: %d %+v", code, bad)
	}
	close(e.gate)
	got := e.awaitCreation(all, st.CreationID)
	if got.Phase != direct.CreatePhaseReady || got.NewID != root || got.RootID != root || got.Uncertain {
		t.Fatalf("ready: %+v (daemon created %s)", got, root)
	}
	// Follow-up on the routed id: add-agent into the new workgroup.
	code, member := e.createAsync(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "codex"}, "slow-2")
	if code != 202 {
		t.Fatalf("add-agent: %d %+v", code, member)
	}
	added := e.awaitCreation(all, member.CreationID)
	if added.Phase != direct.CreatePhaseReady || added.RootID != root {
		t.Fatalf("add-agent ready: %+v", added)
	}
	inv := e.eventually(all, "member", func(inv map[string]direct.Session) bool { return inv[added.NewID].RootID == root })
	if !e.h.Running(added.NewID) {
		t.Fatalf("member %+v not running", inv[added.NewID])
	}
	// Replays in both modes answer from the record; no new daemon creation.
	if code, r, _ := e.act(all, "", direct.VerbNewWorkgroup, fields, "slow-1"); code != 200 || r.NewID != root {
		t.Fatalf("sync replay: %d %+v", code, r)
	}
	if _, again := e.createAsync(all, "", direct.VerbNewWorkgroup, fields, "slow-1"); again.NewID != root || again.CreationID != st.CreationID {
		t.Fatalf("async replay: %+v", again)
	}
	if n := e.dispatches.Load(); n != 2 {
		t.Fatalf("daemon creations=%d, want 2", n)
	}
}

// Revocation after the creation was sent: the job is drained and its outcome
// recorded as unknown — and the daemon did in fact create the session, which
// is exactly why it is reported uncertain rather than "did not run".
func TestDirectLiveAsyncRevokeAfterSendIsUncertain(t *testing.T) {
	e := newSlowE2E(t)
	defer close(e.gate)
	code, _, err := e.auth.NewCode("e2e", everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.auth.RedeemCode(code, "phone", direct.ClientAndroid)
	if err != nil {
		t.Fatal(err)
	}
	status, st := e.createAsync(tok.AccessToken, "", direct.VerbNewWorkgroupEmpty, map[string]string{"name": "Gone", "repos": ""}, "rv")
	if status != 202 || st.Phase != direct.CreatePhasePreparing {
		t.Fatalf("accepted: %d %+v", status, st)
	}
	root := <-e.created
	if err := e.auth.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if code, _, bad := e.creation(tok.AccessToken, st.CreationID, ""); code != 401 || bad.Code != direct.CodeAuthRevoked {
		t.Fatalf("revoked reader: %d %+v", code, bad)
	}
	other := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})
	if code, _, _ := e.creation(other, st.CreationID, ""); code != 404 {
		t.Fatalf("another grant saw the creation: %d", code)
	}
	e.eventually(other, "the daemon's effect", func(inv map[string]direct.Session) bool { return inv[root].IsRoot })
}
