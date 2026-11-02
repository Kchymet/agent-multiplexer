package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"amux/internal/daemon"
	"amux/internal/hostbackend"
)

// Acceptance matrix for asynchronous creation (docs/direct-async-create.md).

func init() { asyncInitialWait = 50 * time.Millisecond }

func asyncBody(verb string, fields map[string]string, key string) string {
	if fields == nil {
		fields = map[string]string{}
	}
	b, _ := json.Marshal(ActionRequest{Verb: verb, Fields: fields, IdemKey: key, Async: true})
	return string(b)
}

func createPath(sid string) string {
	if sid == "" {
		return PathWorkgroups
	}
	return PathSession + "/" + sid + "/actions"
}

// postAsync sends one async creation and decodes either body shape.
func postAsync(s *Server, tok, sid, verb string, fields map[string]string, key string) (int, CreationStatus, string) {
	w := do(s, "POST", createPath(sid), tok, asyncBody(verb, fields, key))
	var st CreationStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	var e APIError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code == 202 && strings.Contains(w.Body.String(), `"ok"`) {
		panic("an async answer carried an ActionResponse field: " + w.Body.String())
	}
	return w.Code, st, e.Code
}

func getStatus(s *Server, tok, id, query string) (int, CreationStatus, string) {
	path := PathCreations + "/" + id
	if query != "" {
		path += "?" + query
	}
	w := do(s, "GET", path, tok, "")
	var st CreationStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	var e APIError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, st, e.Code
}

// awaitTerminal long-polls until the creation is terminal.
func awaitTerminal(t *testing.T, s *Server, tok, id string) CreationStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	after := 0
	for time.Now().Before(deadline) {
		code, st, ec := getStatus(s, tok, id, fmt.Sprintf("wait=5&after=%d", after))
		if code != 200 {
			t.Fatalf("status: %d %s", code, ec)
		}
		if st.Version >= 3 {
			return st
		}
		after = st.Version
	}
	t.Fatal("creation never finished")
	return CreationStatus{}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(time.Millisecond)
	}
}

func gated(f *fakeBackend) chan struct{} {
	g := make(chan struct{})
	f.mu.Lock()
	f.gate = g
	f.mu.Unlock()
	return g
}

// occupySlots holds every daemon slot so new jobs stay queued.
func occupySlots(s *Server) func() {
	for range cap(s.jobs.slots) {
		s.jobs.slots <- struct{}{}
	}
	return func() {
		for range cap(s.jobs.slots) {
			<-s.jobs.slots
		}
	}
}

// 1, 2: a creation slower than the sync bound answers 202 at once and is
// followed to ready with new_id/root_id; capabilities advertise it.
func TestAsyncCreateSlowThenReady(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)

	w := do(s, "GET", PathCapabilities, tok.AccessToken, "")
	if !strings.Contains(w.Body.String(), `"async_create":true`) {
		t.Fatalf("capabilities: %s", w.Body.String())
	}
	gate := gated(f)
	f.newID = "root-1"
	code, st, ec := postAsync(s, tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"name": "N", "repos": "api"}, "k1")
	if code != 202 || st.Phase != CreatePhasePreparing || st.Version != 2 || !validCreationID(st.CreationID) || st.Verb != VerbNewWorkgroup || st.NewID != "" {
		t.Fatalf("accepted: %d %s %+v", code, ec, st)
	}
	close(gate)
	got := awaitTerminal(t, s, tok.AccessToken, st.CreationID)
	if got.Phase != CreatePhaseReady || got.NewID != "root-1" || got.RootID != "root-1" || got.CreationID != st.CreationID || got.Uncertain {
		t.Fatalf("ready: %+v", got)
	}
	f.newID = "agent-1"
	code, st, _ = postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k2")
	if code != 202 {
		t.Fatalf("add-agent: %d", code)
	}
	got = awaitTerminal(t, s, tok.AccessToken, st.CreationID)
	if got.Phase != CreatePhaseReady || got.NewID != "agent-1" || got.RootID != "wg1" {
		t.Fatalf("add-agent ready: %+v", got)
	}
	if f.applies.Load() != 2 || s.jobs.pending() != 0 {
		t.Fatalf("applies=%d pending=%d", f.applies.Load(), s.jobs.pending())
	}
	// A steer-only grant sees no async_create and cannot read creations.
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	if w := do(s, "GET", PathCapabilities, steer.AccessToken, ""); strings.Contains(w.Body.String(), "async_create") {
		t.Fatalf("steer capabilities: %s", w.Body.String())
	}
	if code, _, ec := getStatus(s, steer.AccessToken, st.CreationID, ""); code != 403 || ec != CodeDenied {
		t.Fatalf("status without create: %d %s", code, ec)
	}
}

// 3, 15: neither client disconnect nor access-token expiry cancels an admitted
// job; a fresh token reads its outcome. Polling never refreshes the grant.
func TestAsyncCreateSurvivesDisconnectAndTokenExpiry(t *testing.T) {
	clk := &clock{t: time.Now()}
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, clk.now)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	gate := gated(f)
	f.newID = "agent-2"

	// The client goes away while the POST waits.
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", createPath("wg1"), strings.NewReader(asyncBody(VerbAddAgent, nil, "dc"))).WithContext(ctx)
	r.Host = testHost
	r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	r.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { s.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	waitFor(t, "dispatch", func() bool { return f.activeApplies.Load() == 1 })
	cancel()
	<-done

	lastRefresh := func() int64 { a.mu.Lock(); defer a.mu.Unlock(); return a.state.Grants[tok.ClientID].LastRefreshAt }
	before := lastRefresh()
	clk.add(MaxAccessTTL + time.Minute) // the access token expires
	if code, _, ec := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "dc"); code != 401 || ec != CodeAuthExpired {
		t.Fatalf("expired token: %d %s", code, ec)
	}
	close(gate)
	waitFor(t, "job end", func() bool { return s.jobs.pending() == 0 })
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	code, st, _ := postAsync(s, next.AccessToken, "wg1", VerbAddAgent, nil, "dc")
	if code != 202 || st.Phase != CreatePhaseReady || st.NewID != "agent-2" || st.RootID != "wg1" {
		t.Fatalf("after reconnect: %d %+v", code, st)
	}
	after := lastRefresh()
	for range 3 {
		getStatus(s, next.AccessToken, st.CreationID, "")
	}
	if lastRefresh() != after || after < before {
		t.Fatal("status polling moved the grant's refresh time")
	}
	if f.applies.Load() != 1 {
		t.Fatalf("applies=%d", f.applies.Load())
	}
}

// 4, 5: one identity per (grant, idem_key) across sync and async, in flight,
// done and ambiguous; never a second Apply, never a changed body.
func TestAsyncSameKeyIdentityAcrossModes(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	fields := map[string]string{"repos": "api"}

	// Async first, in flight.
	gate := gated(f)
	f.newID = "agent-3"
	_, first, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "same")
	if _, again, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "same"); again.CreationID != first.CreationID || again.Phase != CreatePhasePreparing {
		t.Fatalf("async re-POST: %+v vs %+v", again, first)
	}
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "same"); code != 409 || ec != CodeIdemInProgress {
		t.Fatalf("sync while async in flight: %d %s", code, ec)
	}
	for _, sync := range []bool{false, true} {
		var code int
		var ec string
		if sync {
			code, ec, _ = act(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": ""}, "same")
		} else {
			code, _, ec = postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": ""}, "same")
		}
		if code != 422 || ec != CodeIdemConflict {
			t.Fatalf("changed body (sync=%v): %d %s", sync, code, ec)
		}
	}
	close(gate)
	awaitTerminal(t, s, tok.AccessToken, first.CreationID)
	if code, _, r := act(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "same"); code != 200 || r.NewID != "agent-3" || r.RootID != "wg1" {
		t.Fatalf("sync replay of async: %d %+v", code, r)
	}
	if _, again, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "same"); again.CreationID != first.CreationID || again.Phase != CreatePhaseReady {
		t.Fatalf("async replay: %+v", again)
	}

	// Sync first, completed: async adopts it under one stable id.
	f.mu.Lock()
	f.gate = nil
	f.mu.Unlock()
	f.newID = "agent-4"
	if code, _, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncdone"); code != 200 {
		t.Fatal("sync create")
	}
	_, s1, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncdone")
	_, s2, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncdone")
	if s1.Phase != CreatePhaseReady || s1.NewID != "agent-4" || s1.CreationID == "" || s2.CreationID != s1.CreationID {
		t.Fatalf("sync-first adopted: %+v %+v", s1, s2)
	}
	if code, st, _ := getStatus(s, tok.AccessToken, s1.CreationID, ""); code != 200 || st.NewID != "agent-4" {
		t.Fatalf("sync-first status: %d %+v", code, st)
	}

	// Sync first, in flight: async reports it preparing.
	gate = gated(f)
	f.newID = "agent-5"
	syncDone := make(chan int)
	go func() {
		code, _, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncfly")
		syncDone <- code
	}()
	waitFor(t, "sync dispatch", func() bool { return f.activeApplies.Load() == 1 })
	if _, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncfly"); st.Phase != CreatePhasePreparing {
		t.Fatalf("sync in flight via async: %+v", st)
	}
	close(gate)
	if code := <-syncDone; code != 200 {
		t.Fatalf("sync in flight: %d", code)
	}

	// Sync first, ambiguous: async reports failed and uncertain.
	f.mu.Lock()
	f.gate = nil
	f.mu.Unlock()
	f.applyErr = errors.New("connection reset after dispatch")
	act(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncamb")
	f.applyErr = nil
	if _, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, fields, "syncamb"); st.Phase != CreatePhaseFailed || !st.Uncertain || st.Code != CodeIdemAmbiguous {
		t.Fatalf("sync ambiguous via async: %+v", st)
	}
	if n := f.applies.Load(); n != 4 {
		t.Fatalf("applies=%d, want 4", n)
	}
}

// 6, 7, 8: rejection and unavailability are known outcomes; a post-dispatch
// failure is ambiguous and never re-run.
func TestAsyncOutcomeClassification(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	cases := []struct {
		key       string
		err       error
		newID     string
		code      string
		uncertain bool
	}{
		{"rej", &daemon.RejectedError{Msg: "repository setup failed"}, "", CodeRejected, false},
		{"down", fmt.Errorf("%w: dial", hostbackend.ErrUnavailable), "", CodeUnavailable, false},
		{"lost", errors.New("connection reset after dispatch"), "", CodeIdemAmbiguous, true},
		{"bound", context.DeadlineExceeded, "", CodeIdemAmbiguous, true},
		{"noid", nil, "", CodeIdemAmbiguous, true},
	}
	for _, c := range cases {
		f.applyErr, f.newID = c.err, c.newID
		_, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, c.key)
		got := awaitTerminal(t, s, tok.AccessToken, st.CreationID)
		if got.Phase != CreatePhaseFailed || got.Code != c.code || got.Uncertain != c.uncertain || got.NewID != "" {
			t.Errorf("%s: %+v", c.key, got)
		}
		f.applyErr = nil
		before := f.applies.Load()
		if _, again, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, c.key); again.Code != c.code || f.applies.Load() != before {
			t.Errorf("%s replay ran again or changed: %+v", c.key, again)
		}
	}
}

// 9: revocation during preparing drains the job; its outcome is unknown and
// invisible to any other grant.
func TestAsyncRevokeWhilePreparing(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	gated(f)
	_, st, _ := postAsync(s, tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"name": "x"}, "rv")
	if st.Phase != CreatePhasePreparing {
		t.Fatalf("not preparing: %+v", st)
	}
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if f.activeApplies.Load() != 0 {
		t.Fatal("revoke returned before the job drained")
	}
	waitFor(t, "release", func() bool { return s.jobs.pending() == 0 })
	if code, _, ec := getStatus(s, tok.AccessToken, st.CreationID, ""); code != 401 || ec != CodeAuthRevoked {
		t.Fatalf("revoked reader: %d %s", code, ec)
	}
	a.mu.Lock()
	r := a.state.Grants[tok.ClientID].Idem["rv"]
	a.mu.Unlock()
	if r.State != idemUnknown {
		t.Fatalf("record after revoke: %+v", r)
	}
	other := enroll(t, a, allPerms, allSessions)
	if code, _, ec := getStatus(s, other.AccessToken, st.CreationID, ""); code != 404 || ec != CodeNotFound {
		t.Fatalf("other grant: %d %s", code, ec)
	}
}

// 1 (corrections): queued work re-checks everything before dispatch.
// Revocation, absolute expiry, the idle limit, a catalog narrowing and an
// archive while queued each end it as "did not run", with no daemon call.
func TestAsyncQueuedWorkIsFencedBeforeDispatch(t *testing.T) {
	type setup struct {
		name  string
		life  Lifetimes
		key   string
		apply func(t *testing.T, a *Authority, f *fakeBackend, clk *clock, tok TokenResponse)
		code  string
	}
	cases := []setup{
		{"revoked", Lifetimes{}, "q1", func(t *testing.T, a *Authority, _ *fakeBackend, _ *clock, tok TokenResponse) {
			go func() { _ = a.Revoke(tok.ClientID, "test") }()
		}, CodeAuthRevoked},
		{"absolute expiry", Lifetimes{Absolute: time.Hour}, "q2", func(_ *testing.T, _ *Authority, _ *fakeBackend, clk *clock, _ TokenResponse) {
			clk.add(time.Hour + time.Second)
		}, CodeAuthExpired},
		{"idle limit", Lifetimes{Idle: time.Hour}, "q3", func(_ *testing.T, _ *Authority, _ *fakeBackend, clk *clock, _ TokenResponse) {
			clk.add(time.Hour + time.Second)
		}, CodeAuthExpired},
		{"catalog narrowed", Lifetimes{}, "q4", func(_ *testing.T, _ *Authority, f *fakeBackend, _ *clock, _ TokenResponse) {
			f.mu.Lock()
			f.catalog.Ceilings["wg1"] = []string{}
			f.mu.Unlock()
		}, CodeNotInCatalog},
		{"archived", Lifetimes{}, "q5", func(_ *testing.T, _ *Authority, f *fakeBackend, _ *clock, _ TokenResponse) {
			rows := workflowRows()
			rows[0].Archived = true
			f.setRows(rows...)
		}, CodeArchived},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &clock{t: time.Now()}
			f := workflowFake()
			a := openTestAuthority(t, t.TempDir(), c.life, clk.now)
			s := newTestServer(t, a, f)
			tok := enroll(t, a, allPerms, allSessions)
			free := occupySlots(s)
			code, st, ec := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "api"}, c.key)
			if code != 202 || st.Phase != CreatePhaseQueued || st.Version != 1 {
				t.Fatalf("queued: %d %s %+v", code, ec, st)
			}
			c.apply(t, a, f, clk, tok)
			if c.code == CodeAuthRevoked {
				waitFor(t, "revocation", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.state.Grants[tok.ClientID].RevokedAt != 0 })
			}
			free()
			waitFor(t, "job end", func() bool { return s.jobs.pending() == 0 })
			a.mu.Lock()
			r := *a.state.Grants[tok.ClientID].Idem[c.key]
			a.mu.Unlock()
			v := statusOf(creationView{rec: r}, clk.now())
			if v.Phase != CreatePhaseFailed || v.Code != c.code || v.Uncertain || r.DispatchedAt != 0 {
				t.Fatalf("outcome: %+v record %+v", v, r)
			}
			if f.applies.Load() != 0 {
				t.Fatal("fenced work reached the daemon")
			}
		})
	}
}

// The job's daemon call ends at min(asyncCreateLimit, absolute grant expiry).
func TestHeldLeaseDeadlineIsGrantExpiry(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Absolute: time.Minute}, nil)
	tok := enroll(t, a, allPerms, allSessions)
	l, err := a.Admit(t.Context(), tok.AccessToken, PermCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Done()
	h, err := a.Hold(l, PermCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Done()
	exec, cancel := h.ExecContext(asyncCreateLimit)
	defer cancel()
	dl, ok := exec.Deadline()
	if !ok || time.Until(dl) > time.Minute+time.Second {
		t.Fatalf("deadline %v not bounded by the grant's absolute expiry", time.Until(dl))
	}
	l.Done() // the request's lease ending does not end the job's
	if h.Context().Err() != nil {
		t.Fatal("worker lease ended with the request")
	}
}

// 10: restart. A record admitted but never dispatched did not run; one
// dispatched is ambiguous. Neither runs again, in either mode.
func TestAsyncRecordsAfterRestart(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, allPerms, allSessions)
	l, err := a.Admit(t.Context(), tok.AccessToken, PermCreate)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, key := range []string{"never", "sent"} {
		hash := actionHash("wg1", ActionRequest{Verb: VerbAddAgent, Fields: map[string]string{}})
		if out, _, err := a.BeginAction(l, key, hash, false, stampCreation(VerbAddAgent, true)); err != nil || out != idemProceed {
			t.Fatalf("begin %s: %v %v", key, out, err)
		}
		ids[key] = a.state.Grants[tok.ClientID].Idem[key].CreationID
	}
	if err := a.MarkDispatched(l, "sent"); err != nil {
		t.Fatal(err)
	}
	l.Done()
	a.Close() // crash: neither outcome recorded

	clk := &clock{t: time.Now()}
	a = openTestAuthority(t, root, Lifetimes{}, clk.now)
	// Load settled the never-sent record durably: terminal, known not run.
	a.mu.Lock()
	never, sent := *a.state.Grants[tok.ClientID].Idem["never"], *a.state.Grants[tok.ClientID].Idem["sent"]
	a.mu.Unlock()
	if never.State != idemDone || never.OK || never.Code != CodeUnavailable || sent.State != idemAdmitted {
		t.Fatalf("after load: never=%+v sent=%+v", never, sent)
	}
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	f := workflowFake()
	s := newTestServer(t, a, f)
	if code, st, _ := getStatus(s, next.AccessToken, ids["never"], ""); code != 200 || st.Phase != CreatePhaseFailed || st.Code != CodeUnavailable || st.Uncertain {
		t.Fatalf("undispatched: %d %+v", code, st)
	}
	if code, st, _ := getStatus(s, next.AccessToken, ids["sent"], ""); code != 200 || st.Phase != CreatePhaseFailed || st.Code != CodeIdemAmbiguous || !st.Uncertain {
		t.Fatalf("dispatched: %d %+v", code, st)
	}
	// Every mode agrees: never sent is a known "did not run" (unavailable);
	// sent is ambiguous. Neither runs again.
	want := map[string]struct {
		code   string
		status int
	}{"never": {CodeUnavailable, 503}, "sent": {CodeIdemAmbiguous, 409}}
	for key, w := range want {
		if _, st, _ := postAsync(s, next.AccessToken, "wg1", VerbAddAgent, nil, key); st.CreationID != ids[key] || st.Phase != CreatePhaseFailed || st.Code != w.code {
			t.Fatalf("async retry %s: %+v", key, st)
		}
		if code, ec, _ := act(s, next.AccessToken, "wg1", VerbAddAgent, nil, key); code != w.status || ec != w.code {
			t.Fatalf("sync retry %s: %d %s", key, code, ec)
		}
	}
	if f.applies.Load() != 0 {
		t.Fatal("a restarted record ran again")
	}
	// The settled record is a done record: retention prunes it (freeing its
	// capacity) on the next admission after idemRetention; the ambiguous one
	// is kept forever.
	clk.add(idemRetention + time.Hour)
	later, err := a.Refresh(next.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	f.newID = "agent-r"
	if code, _, _ := act(s, later.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "api"}, "after"); code != 200 {
		t.Fatal("admission after retention")
	}
	a.mu.Lock()
	_, neverKept := a.state.Grants[tok.ClientID].Idem["never"]
	_, sentKept := a.state.Grants[tok.ClientID].Idem["sent"]
	a.mu.Unlock()
	if neverKept || !sentKept {
		t.Fatalf("retention: never kept=%v sent kept=%v", neverKept, sentKept)
	}
	if code, _, _ := getStatus(s, later.AccessToken, ids["never"], ""); code != 404 {
		t.Fatalf("pruned creation: %d", code)
	}
}

// Grant idle lifetime fences a creation queued or running: the worker ends at
// LastRefreshAt+IdleTTL unless a real refresh moved it. Neither the job nor a
// status poll extends it.
func TestAsyncIdleExpiryFencesQueuedAndRunning(t *testing.T) {
	prev := grantRecheck.Swap(int64(2 * time.Millisecond))
	defer grantRecheck.Store(prev)
	recheck := 2 * time.Millisecond
	setup := func(t *testing.T) (*clock, *Authority, *Server, *fakeBackend, TokenResponse) {
		clk := &clock{t: time.Now()}
		f := workflowFake()
		a := openTestAuthority(t, t.TempDir(), Lifetimes{Idle: time.Hour}, clk.now)
		return clk, a, newTestServer(t, a, f), f, enroll(t, a, allPerms, allSessions)
	}
	record := func(a *Authority, tok TokenResponse, key string) idemRecord {
		a.mu.Lock()
		defer a.mu.Unlock()
		return *a.state.Grants[tok.ClientID].Idem[key]
	}

	t.Run("running expires", func(t *testing.T) {
		clk, a, s, f, tok := setup(t)
		gated(f)
		if _, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "run"); st.Phase != CreatePhasePreparing {
			t.Fatalf("not preparing: %+v", st)
		}
		for range 3 { // polls do not refresh
			getStatus(s, tok.AccessToken, newCreationID(), "")
		}
		clk.add(time.Hour + time.Second)
		waitFor(t, "idle fence", func() bool { return s.jobs.pending() == 0 })
		if r := record(a, tok, "run"); r.State != idemUnknown {
			t.Fatalf("idle-ended running creation: %+v", r)
		}
		if f.activeApplies.Load() != 0 {
			t.Fatal("daemon call still waited on after idle expiry")
		}
	})
	t.Run("running extended by refresh", func(t *testing.T) {
		clk, a, s, f, tok := setup(t)
		gate := gated(f)
		f.newID = "agent-idle"
		_, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "ext")
		clk.add(50 * time.Minute)
		next, err := a.Refresh(tok.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		clk.add(20 * time.Minute) // past the original idle deadline, inside the refreshed one
		time.Sleep(20 * recheck)
		if s.jobs.pending() != 1 || f.activeApplies.Load() != 1 {
			t.Fatal("a refreshed grant's running creation was cut")
		}
		next, err = a.Refresh(next.RefreshToken) // the reader's own token has expired meanwhile
		if err != nil {
			t.Fatal(err)
		}
		close(gate)
		if got := awaitTerminal(t, s, next.AccessToken, st.CreationID); got.Phase != CreatePhaseReady {
			t.Fatalf("refreshed: %+v", got)
		}
	})
	t.Run("queued extended by refresh", func(t *testing.T) {
		clk, a, s, f, tok := setup(t)
		f.newID = "agent-q"
		free := occupySlots(s)
		_, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "qext")
		clk.add(50 * time.Minute)
		next, err := a.Refresh(tok.RefreshToken)
		if err != nil {
			t.Fatal(err)
		}
		clk.add(20 * time.Minute)
		time.Sleep(20 * recheck)
		free()
		if next, err = a.Refresh(next.RefreshToken); err != nil {
			t.Fatal(err)
		}
		if got := awaitTerminal(t, s, next.AccessToken, st.CreationID); got.Phase != CreatePhaseReady || f.applies.Load() != 1 {
			t.Fatalf("queued after refresh: %+v", got)
		}
	})
	t.Run("queued expires", func(t *testing.T) {
		clk, a, s, f, tok := setup(t)
		free := occupySlots(s)
		postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "qexp")
		clk.add(time.Hour + time.Second)
		waitFor(t, "idle fence", func() bool { return s.jobs.pending() == 0 })
		free()
		if r := record(a, tok, "qexp"); r.State != idemDone || r.Code != CodeAuthExpired || r.DispatchedAt != 0 || f.applies.Load() != 0 {
			t.Fatalf("idle-ended queued creation: %+v", r)
		}
	})
}

// A POST that would disclose a status after its own access ended answers the
// auth code instead; the creation is unaffected and a fresh token reads it.
func TestAsyncPostAppliesAuthEndedBeforeDisclosure(t *testing.T) {
	prev := asyncInitialWait
	asyncInitialWait = 3 * time.Second
	defer func() { asyncInitialWait = prev }()
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	gate := gated(f)
	f.newID = "agent-ae"
	w := do(s, "POST", createPath("wg1"), tok.AccessToken, asyncBody(VerbAddAgent, nil, "ae"))
	if w.Code != 401 || apiCode(t, w) != CodeAuthExpired || strings.Contains(w.Body.String(), "creation_id") {
		t.Fatalf("expired during the wait: %d %s", w.Code, w.Body.String())
	}
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	_, st, _ := postAsync(s, next.AccessToken, "wg1", VerbAddAgent, nil, "ae")
	if st.Phase != CreatePhasePreparing {
		t.Fatalf("job did not survive the reader's expiry: %+v", st)
	}
	close(gate)
	if got := awaitTerminal(t, s, next.AccessToken, st.CreationID); got.Phase != CreatePhaseReady || got.NewID != "agent-ae" {
		t.Fatalf("after refresh: %+v", got)
	}
}

// 11, 12: authorization and synchronous refusals record nothing.
func TestAsyncAuthorizationAndPrechecks(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	scoped := enroll(t, a, allPerms, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	cases := []struct {
		name, tok, sid, verb string
		fields               map[string]string
		code                 int
		ec                   string
	}{
		{"no create", steer.AccessToken, "wg1", VerbAddAgent, nil, 403, CodeDenied},
		{"scoped root", scoped.AccessToken, "", VerbNewWorkgroup, map[string]string{"name": "x"}, 403, CodeDenied},
		{"out of scope", scoped.AccessToken, "wg3", VerbAddAgent, nil, 404, CodeNotFound},
		{"console", tok.AccessToken, "console", VerbAddAgent, nil, 404, CodeNotFound},
		{"archived", tok.AccessToken, "wg2", VerbAddAgent, nil, 409, CodeArchived},
		{"catalog", tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "web"}, 422, CodeNotInCatalog},
		{"harness", tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"agent": "hermes", "repos": "api"}, 422, CodeNotInCatalog},
		{"non-creation verb", tok.AccessToken, "wg1", VerbRename, map[string]string{"name": "n"}, 400, CodeBadRequest},
		{"prompt", tok.AccessToken, "a1", VerbPrompt, map[string]string{"text": "x"}, 400, CodeBadRequest},
	}
	for i, c := range cases {
		key := fmt.Sprintf("p%d", i)
		code, _, ec := postAsync(s, c.tok, c.sid, c.verb, c.fields, key)
		if code != c.code || ec != c.ec {
			t.Errorf("%s: %d %s", c.name, code, ec)
		}
		a.mu.Lock()
		for _, g := range a.state.Grants {
			if g.Idem[key] != nil {
				t.Errorf("%s recorded its key", c.name)
			}
		}
		a.mu.Unlock()
	}
	if f.applies.Load() != 0 || s.jobs.pending() != 0 {
		t.Fatal("refused creations reached the daemon or kept a reservation")
	}
	for _, bad := range []string{"nope", "crt_short", newCreationID()} {
		if code, _, _ := getStatus(s, tok.AccessToken, bad, ""); code != 404 {
			t.Errorf("status %q: %d", bad, code)
		}
	}
}

// 13: reservations bound pending work and are released on every path.
func TestAsyncReservationsAreBoundedAndReleased(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	prev := maxAsyncPerGrant
	maxAsyncPerGrant = 1
	defer func() { maxAsyncPerGrant = prev }()
	gate := gated(f)
	if code, _, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "r1"); code != 202 {
		t.Fatal("first")
	}
	if code, _, ec := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "r2"); code != 429 || ec != CodeRateLimited {
		t.Fatalf("over the grant cap: %d %s", code, ec)
	}
	if a.state.Grants[tok.ClientID].Idem["r2"] != nil {
		t.Fatal("a refused creation was recorded")
	}
	// Another grant has its own budget.
	other := enroll(t, a, allPerms, allSessions)
	if code, _, _ := postAsync(s, other.AccessToken, "wg1", VerbAddAgent, nil, "r3"); code != 202 {
		t.Fatal("other grant")
	}
	close(gate)
	waitFor(t, "release", func() bool { return s.jobs.pending() == 0 })
	// The record-capacity failure path releases its reservation too.
	prevIdem := maxIdemPerGrant
	maxIdemPerGrant = 1
	defer func() { maxIdemPerGrant = prevIdem }()
	if code, _, ec := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "r4"); code != 409 || ec != CodeCapacity {
		t.Fatalf("capacity: %d %s", code, ec)
	}
	if s.jobs.pending() != 0 {
		t.Fatal("a failed admission kept its reservation")
	}
}

// 14: a long-poll from a known version returns on the next transition and
// not before; without wait it answers at once.
func TestAsyncLongPollFromVersion(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	gate := gated(f)
	f.newID = "agent-9"
	_, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "lp")
	if code, now, _ := getStatus(s, tok.AccessToken, st.CreationID, ""); code != 200 || now.Version != 2 {
		t.Fatalf("immediate: %d %+v", code, now)
	}
	start := time.Now()
	if _, same, _ := getStatus(s, tok.AccessToken, st.CreationID, "wait=1&after=2"); same.Version != 2 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("returned before a change: %+v after %v", same, time.Since(start))
	}
	got := make(chan CreationStatus, 1)
	go func() { _, v, _ := getStatus(s, tok.AccessToken, st.CreationID, "wait=10&after=2"); got <- v }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	select {
	case v := <-got:
		if v.Phase != CreatePhaseReady || v.NewID != "agent-9" {
			t.Fatalf("long-poll: %+v", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("long-poll missed the transition")
	}
	if code, _, ec := getStatus(s, tok.AccessToken, st.CreationID, "wait=x"); code != 400 || ec != CodeBadRequest {
		t.Fatalf("bad wait: %d %s", code, ec)
	}
}

// 16: an old client (no async) sees byte-identical behavior, and an older
// record without a creation id is adopted once and keeps that id across a
// restart.
func TestAsyncOldClientAndStoreCompat(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, allPerms, allSessions)
	f := workflowFake()
	f.newID = "agent-old"
	s := newTestServer(t, a, f)
	w := do(s, "POST", createPath("wg1"), tok.AccessToken, body(VerbAddAgent, nil, "old"))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"ok":true,"new_id":"agent-old","root_id":"wg1"}` {
		t.Fatalf("sync answer changed: %d %s", w.Code, w.Body.String())
	}
	a.mu.Lock()
	rec := a.state.Grants[tok.ClientID].Idem["old"]
	if rec.CreationID != "" || rec.Async || rec.DispatchedAt != 0 {
		t.Fatalf("the sync path wrote creation fields: %+v", rec)
	}
	a.mu.Unlock()
	_, st, _ := postAsync(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "old")
	if st.Phase != CreatePhaseReady || st.NewID != "agent-old" {
		t.Fatalf("adopted: %+v", st)
	}
	a.Close()
	a = openTestAuthority(t, root, Lifetimes{}, nil)
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, a, f)
	if code, again, _ := getStatus(s, next.AccessToken, st.CreationID, ""); code != 200 || again.NewID != "agent-old" {
		t.Fatalf("adopted id after restart: %d %+v", code, again)
	}
	if f.applies.Load() != 1 {
		t.Fatalf("applies=%d", f.applies.Load())
	}
}
