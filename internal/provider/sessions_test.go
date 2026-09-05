package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/wiretls"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// mutableSource is a test session rail whose contents the test can swap under a
// lock, so a "rail change" can be driven and the resulting push asserted.
type mutableSource struct {
	mu   sync.Mutex
	sess []core.Session
}

func (m *mutableSource) set(s []core.Session) {
	m.mu.Lock()
	m.sess = s
	m.mu.Unlock()
}

func (m *mutableSource) poll(context.Context) ([]core.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.Session, len(m.sess))
	copy(out, m.sess)
	return out, nil
}

// recordingApply captures the last core.Action it was handed and returns a
// per-action newId, so a verb round-trip can assert both the mapping and the
// echoed result.
type recordingApply struct {
	mu     sync.Mutex
	called bool
	got    core.Action
}

func (r *recordingApply) apply(_ context.Context, a core.Action) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	r.got = a
	return "new-" + a.Action + "-" + a.ID, nil
}

func (r *recordingApply) last() (core.Action, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got, r.called
}

// subscribe tells the provider to start publishing on this connection.
func subscribe(t *testing.T, oc *harnessproto.Conn) {
	t.Helper()
	if err := oc.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MSessionsSubscribe}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// readSessions reads the next frame, requiring it to be a sessions snapshot.
func readSessions(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m := readFrame(t, oc)
	if m.Type != harnessproto.HSessions {
		t.Fatalf("frame = %q, want sessions", m.Type)
	}
	return m
}

// TestNegotiationAdvertisesSessions proves the feature is advertised only when
// opted in with an inventory source.
func TestNegotiationAdvertisesSessions(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		conns := make(chan net.Conn, 1)
		src := &mutableSource{}
		p := newFast(Config{
			Orchestrator: "pipe", Dial: pipeDialer(conns),
			Features:        []string{"gpu"},
			PublishSessions: true, Sessions: src.poll,
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.Run(ctx)

		oc := harnessproto.NewConn(<-conns)
		reg := expectRegister(t, oc)
		if !hasFeature(reg, harnessproto.SessionsFeature) {
			t.Fatalf("features = %v, want to include %q", reg.Capabilities.Features, harnessproto.SessionsFeature)
		}
		if !hasFeature(reg, "gpu") {
			t.Fatalf("opaque config feature dropped: %v", reg.Capabilities.Features)
		}
	})

	t.Run("off", func(t *testing.T) {
		conns := make(chan net.Conn, 1)
		p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns), Features: []string{"gpu"}})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.Run(ctx)

		oc := harnessproto.NewConn(<-conns)
		reg := expectRegister(t, oc)
		if hasFeature(reg, harnessproto.SessionsFeature) {
			t.Fatalf("advertised %q while disabled: %v", harnessproto.SessionsFeature, reg.Capabilities.Features)
		}
	})

	t.Run("no-source", func(t *testing.T) {
		// PublishSessions set but no Sessions func: the feature stays inactive.
		conns := make(chan net.Conn, 1)
		p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns), PublishSessions: true})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go p.Run(ctx)

		oc := harnessproto.NewConn(<-conns)
		reg := expectRegister(t, oc)
		if hasFeature(reg, harnessproto.SessionsFeature) {
			t.Fatalf("advertised %q with no inventory source", harnessproto.SessionsFeature)
		}
	})
}

// TestPublishOnSubscribeAndChange proves an initial snapshot on subscribe and a
// second snapshot (next seq) after the rail changes.
func TestPublishOnSubscribeAndChange(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "a1", Title: "one", Section: "workgroups", State: "running", Status: "running · 1 agent"}})
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)

	first := readSessions(t, oc)
	if first.Seq != 1 || len(first.Sessions) != 1 || first.Sessions[0].ID != "a1" {
		t.Fatalf("initial snapshot = seq %d %+v", first.Seq, first.Sessions)
	}

	// Drive a rail change; the next snapshot must reflect it with the next seq.
	src.set([]core.Session{
		{ID: "a1", Title: "one", Section: "workgroups", State: "running", Status: "running · 1 agent"},
		{ID: "a2", Title: "two", Section: "workgroups", State: "waiting", Status: "waiting · needs input"},
	})
	second := readSessions(t, oc)
	if second.Seq != 2 || len(second.Sessions) != 2 || second.Sessions[1].ID != "a2" {
		t.Fatalf("post-change snapshot = seq %d %+v", second.Seq, second.Sessions)
	}
}

// TestSessionActionVerbs round-trips every accepted verb and checks it maps to
// the expected daemon core.Action and echoes an ok result with the created id.
func TestSessionActionVerbs(t *testing.T) {
	cases := []struct {
		verb   string
		id     string
		fields map[string]string
		want   core.Action
	}{
		{harnessproto.VerbNewWorkgroup, "", map[string]string{"name": "pay", "repos": "api,web"},
			core.Action{Action: "new-workgroup", Fields: map[string]string{"name": "pay", "repos": "api,web"}}},
		{harnessproto.VerbAddAgent, "root1", map[string]string{"repos": "api"},
			core.Action{Action: "add-agent", ID: "root1", Fields: map[string]string{"repos": "api"}}},
		{harnessproto.VerbRename, "a1", map[string]string{"name": "renamed"},
			core.Action{Action: "rename", ID: "a1", Fields: map[string]string{"name": "renamed"}}},
		{harnessproto.VerbArchive, "a1", nil,
			core.Action{Action: "set-archived", ID: "a1", Fields: map[string]string{"archived": "true"}}},
		{harnessproto.VerbUnarchive, "a1", nil,
			core.Action{Action: "set-archived", ID: "a1", Fields: map[string]string{"archived": "false"}}},
		{harnessproto.VerbStart, "a1", nil,
			core.Action{Action: core.ActionStart, ID: "a1"}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			conns := make(chan net.Conn, 1)
			rec := &recordingApply{}
			src := &mutableSource{}
			p := newFast(Config{
				Orchestrator: "pipe", Dial: pipeDialer(conns),
				PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go p.Run(ctx)

			oc := harnessproto.NewConn(<-conns)
			accept(t, oc, 2, nil, 60)
			if err := oc.WriteMux(harnessproto.MuxMsg{
				Type: harnessproto.MSessionAction, ReqID: "r1", Action: tc.verb, ID: tc.id, Fields: tc.fields,
			}); err != nil {
				t.Fatal(err)
			}
			res := readResult(t, oc)
			if res.ReqID != "r1" || !res.OK {
				t.Fatalf("result = %+v, want ok with reqId r1", res)
			}
			if res.NewID != "new-"+tc.want.Action+"-"+tc.want.ID {
				t.Fatalf("newId = %q", res.NewID)
			}
			got, called := rec.last()
			if !called || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("apply got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSessionActionExcludedVerb proves any verb outside the fixed set — notably a
// pane/terminal verb — is rejected with "unsupported" and never reaches ApplyAction.
func TestSessionActionExcludedVerb(t *testing.T) {
	for _, verb := range []string{"spawn", "input", "resize", "kill", "delete", "pane.open", "move"} {
		t.Run(verb, func(t *testing.T) {
			conns := make(chan net.Conn, 1)
			rec := &recordingApply{}
			src := &mutableSource{}
			p := newFast(Config{
				Orchestrator: "pipe", Dial: pipeDialer(conns),
				PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go p.Run(ctx)

			oc := harnessproto.NewConn(<-conns)
			accept(t, oc, 2, nil, 60)
			if err := oc.WriteMux(harnessproto.MuxMsg{
				Type: harnessproto.MSessionAction, ReqID: "r2", Action: verb, ID: "a1",
			}); err != nil {
				t.Fatal(err)
			}
			res := readResult(t, oc)
			if res.ReqID != "r2" || res.OK || res.Error != harnessproto.ErrUnsupported {
				t.Fatalf("result = %+v, want ok=false error=%q", res, harnessproto.ErrUnsupported)
			}
			if _, called := rec.last(); called {
				t.Fatal("excluded verb reached ApplyAction")
			}
		})
	}
}

// TestSteeringVerbsRouteToSteer proves the four steering verbs (spec §3.1) reach
// the daemon as one core.ActionSteer naming which verb, with their wire fields
// carried through under the same keys — and that a successful steer answers
// "accepted", not "applied", because the agent has been handed the verb, not
// finished it.
func TestSteeringVerbsRouteToSteer(t *testing.T) {
	cases := []struct {
		verb   string
		fields map[string]string
		want   map[string]string
	}{
		{harnessproto.VerbPrompt,
			map[string]string{harnessproto.FieldText: "run the tests"},
			map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "run the tests"}},
		{harnessproto.VerbInterject,
			map[string]string{harnessproto.FieldText: "skip the flaky one"},
			map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: "skip the flaky one"}},
		{harnessproto.VerbStop, nil,
			map[string]string{core.SteerVerb: core.SteerStop}},
		{harnessproto.VerbPermission,
			map[string]string{
				harnessproto.FieldRequestID: "perm-9",
				harnessproto.FieldDecision:  harnessproto.DecisionDeny,
				harnessproto.FieldReason:    "writes outside the worktree",
				"nosuchfield":               "dropped",
			},
			map[string]string{
				core.SteerVerb:      core.SteerPermission,
				core.SteerRequestID: "perm-9",
				core.SteerDecision:  core.SteerDeny,
				core.SteerReason:    "writes outside the worktree",
			}},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			conns := make(chan net.Conn, 1)
			rec := &recordingApply{}
			src := &mutableSource{}
			p := newFast(Config{
				Orchestrator: "pipe", Dial: pipeDialer(conns),
				PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go p.Run(ctx)

			oc := harnessproto.NewConn(<-conns)
			accept(t, oc, 2, nil, 60)
			if err := oc.WriteMux(harnessproto.MuxMsg{
				Type: harnessproto.MSessionAction, ReqID: "r3", Action: tc.verb, ID: "a1",
				Fields: tc.fields,
			}); err != nil {
				t.Fatal(err)
			}
			res := readResult(t, oc)
			if res.ReqID != "r3" || !res.OK {
				t.Fatalf("result = %+v, want ok", res)
			}
			if res.Result != harnessproto.ResultAccepted {
				t.Fatalf("result disposition = %q, want %q", res.Result, harnessproto.ResultAccepted)
			}
			// The boolean rides with the disposition, never instead of it: a `prompt`
			// to a stopped agent is answered before the runtime has even started, so a
			// consumer has to be able to see "not finished" without matching strings.
			if !res.Accepted {
				t.Fatalf("result = %+v, want the accepted flag set alongside the disposition", res)
			}
			got, called := rec.last()
			if !called {
				t.Fatal("steering verb never reached ApplyAction")
			}
			want := core.Action{Action: core.ActionSteer, ID: "a1", Fields: tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("apply got %+v, want %+v", got, want)
			}
		})
	}
}

// TestLifecycleVerbsReportApplied is the other half of the disposition: a verb
// that finished must not claim to be merely accepted, or an orchestrator would
// wait forever for an asynchronous effect that already happened.
func TestLifecycleVerbsReportApplied(t *testing.T) {
	conns := make(chan net.Conn, 1)
	rec := &recordingApply{}
	src := &mutableSource{}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "r4", Action: harnessproto.VerbArchive, ID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	res := readResult(t, oc)
	if !res.OK || res.Result != harnessproto.ResultApplied {
		t.Fatalf("result = %+v, want ok with %q", res, harnessproto.ResultApplied)
	}
	if res.Accepted {
		t.Fatalf("result = %+v, want the accepted flag clear on a finished verb", res)
	}
}

// TestSessionActionUnimplementedVerb keeps the two rejections distinct: a verb
// that IS in the accepted set but has no handler here answers "unsupported verb",
// not "unsupported", so an orchestrator reads "this daemon is older than the
// verb" rather than "never valid". Every published verb is implemented today, so
// the test registers a future one to exercise the branch.
func TestSessionActionUnimplementedVerb(t *testing.T) {
	const future = "teleport"
	harnessproto.SessionVerbs[future] = true
	t.Cleanup(func() { delete(harnessproto.SessionVerbs, future) })

	conns := make(chan net.Conn, 1)
	rec := &recordingApply{}
	src := &mutableSource{}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "r5", Action: future, ID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	res := readResult(t, oc)
	if res.ReqID != "r5" || res.OK || res.Error != harnessproto.ErrUnsupportedVerb {
		t.Fatalf("result = %+v, want ok=false error=%q", res, harnessproto.ErrUnsupportedVerb)
	}
	if _, called := rec.last(); called {
		t.Fatal("unimplemented verb reached ApplyAction")
	}
}

// TestReadOnlyRejectsVerbs proves read-only publishing accepts inventory but
// rejects every verb — the steering verbs of spec §3.1 exactly as much as the
// lifecycle ones.
func TestReadOnlyRejectsVerbs(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	// No ApplyAction and ReadOnlySessions set: verbs must be refused.
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, ReadOnlySessions: true, Sessions: src.poll,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if !hasFeature(reg, harnessproto.SessionsFeature) {
		t.Fatal("read-only still advertises inventory publishing")
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: true, Version: 2, HeartbeatSeconds: 1, GraceSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	// Every verb, lifecycle and steering alike: read-only means the orchestrator
	// can watch this machine and change nothing on it, so a steering verb must not
	// become a back door into a running agent.
	verbs := []struct {
		verb   string
		fields map[string]string
	}{
		{harnessproto.VerbRename, map[string]string{"name": "x"}},
		{harnessproto.VerbPrompt, map[string]string{harnessproto.FieldText: "go"}},
		{harnessproto.VerbInterject, map[string]string{harnessproto.FieldText: "wait"}},
		{harnessproto.VerbStop, nil},
		{harnessproto.VerbPermission, map[string]string{
			harnessproto.FieldRequestID: "perm-9", harnessproto.FieldDecision: harnessproto.DecisionAllow,
		}},
	}
	for _, v := range verbs {
		if err := oc.WriteMux(harnessproto.MuxMsg{
			Type: harnessproto.MSessionAction, ReqID: "r-" + v.verb, Action: v.verb, ID: "a1",
			Fields: v.fields,
		}); err != nil {
			t.Fatal(err)
		}
		res := readResult(t, oc)
		if res.OK || res.Error == "" {
			t.Fatalf("%s: result = %+v, want a read-only rejection", v.verb, res)
		}
	}
}

// TestReconnectRepublishesSnapshot proves per-connection seq resets and a fresh
// full snapshot is published after a reconnect.
func TestReconnectRepublishesSnapshot(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "a1", Title: "one", Section: "workgroups"}})
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc1 := harnessproto.NewConn(<-conns)
	accept(t, oc1, 2, nil, 60)
	subscribe(t, oc1)
	if m := readSessions(t, oc1); m.Seq != 1 {
		t.Fatalf("first connection initial seq = %d, want 1", m.Seq)
	}
	oc1.Close() // drop the connection

	oc2 := harnessproto.NewConn(<-conns)
	accept(t, oc2, 2, nil, 60)
	subscribe(t, oc2)
	m := readSessions(t, oc2)
	if m.Seq != 1 || len(m.Sessions) != 1 || m.Sessions[0].ID != "a1" {
		t.Fatalf("reconnect snapshot = seq %d %+v, want fresh seq 1", m.Seq, m.Sessions)
	}
}

// TestSessionActionVerbTLS runs one verb round-trip over a real TLS connection.
func TestSessionActionVerbTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := genCert(t, dir)
	srvCfg, err := wiretls.ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	rec := &recordingApply{}
	src := &mutableSource{}
	p := New(Config{
		Orchestrator: "tls://" + ln.Addr().String(), CAFile: certFile,
		PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
	})
	p.hbScale = time.Hour
	p.backoffMin = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-accepted)
	reg := expectRegister(t, oc)
	if !hasFeature(reg, harnessproto.SessionsFeature) {
		t.Fatal("TLS register did not advertise sessions feature")
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: true, Version: 2, HeartbeatSeconds: 1, GraceSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "tls1", Action: harnessproto.VerbNewWorkgroup,
		Fields: map[string]string{"name": "pay"},
	}); err != nil {
		t.Fatal(err)
	}
	res := readResult(t, oc)
	if res.ReqID != "tls1" || !res.OK {
		t.Fatalf("TLS result = %+v", res)
	}
	got, called := rec.last()
	if !called || got.Action != "new-workgroup" || got.Fields["name"] != "pay" {
		t.Fatalf("TLS apply got %+v", got)
	}
}

// ---- helpers ----

func hasFeature(reg harnessproto.HarnessMsg, want string) bool {
	if reg.Capabilities == nil {
		return false
	}
	for _, f := range reg.Capabilities.Features {
		if f == want {
			return true
		}
	}
	return false
}

func readResult(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m := readFrame(t, oc)
	if m.Type != harnessproto.HSessionResult {
		t.Fatalf("frame = %q, want session-result", m.Type)
	}
	return m
}

// recordingLog collects the provider's log lines for assertions.
type recordingLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// find returns the first line containing sub, or "".
func (l *recordingLog) find(sub string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return s
		}
	}
	return ""
}

func (l *recordingLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// TestSessionActionIsLogged covers the observability half: before it, the
// provider logged only dial/register/disconnect, so "did the prompt I sent from
// the web actually reach this machine?" had no answer on the box — the first
// question anyone asks about a relay that looks stuck. Every verb now leaves one
// line naming the session, the verb, how it came out and how long it took.
//
// The negative half matters just as much: the line must never carry `fields`.
// That is where the prompt text and a permission prompt's reason live — the
// user's own words, which have no business in a log just because they passed
// through this process.
func TestSessionActionIsLogged(t *testing.T) {
	conns := make(chan net.Conn, 1)
	rec := &recordingApply{}
	src := &mutableSource{}
	logs := &recordingLog{}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns), Logf: logs.logf,
		PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)

	const secret = "the user's private prompt text"
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "r1", Action: harnessproto.VerbPrompt, ID: "a1",
		Fields: map[string]string{harnessproto.FieldText: secret},
	}); err != nil {
		t.Fatal(err)
	}
	if res := readResult(t, oc); !res.OK {
		t.Fatalf("result = %+v, want ok", res)
	}

	line := logs.find("session-action prompt")
	if line == "" {
		t.Fatalf("no line for the relayed prompt; logged: %v", logs.all())
	}
	for _, want := range []string{"id=a1", harnessproto.ResultAccepted, " in "} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q should contain %q", line, want)
		}
	}
	for _, l := range logs.all() {
		if strings.Contains(l, secret) {
			t.Fatalf("the prompt text reached the log: %q", l)
		}
	}

	// A refusal is logged too, with the reason — a verb that never arrives and one
	// that arrived and was rejected look identical otherwise.
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "r2", Action: "spawn", ID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	if res := readResult(t, oc); res.OK {
		t.Fatalf("result = %+v, want the pane verb refused", res)
	}
	line = logs.find("session-action spawn")
	if line == "" {
		t.Fatalf("no line for the refused verb; logged: %v", logs.all())
	}
	if !strings.Contains(line, "failed") || !strings.Contains(line, harnessproto.ErrUnsupported) {
		t.Errorf("refusal line %q should say it failed and why", line)
	}
}
