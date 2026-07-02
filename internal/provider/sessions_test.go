package provider

import (
	"context"
	"crypto/tls"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/harnessproto"
	"amux/internal/wiretls"
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

// TestReadOnlyRejectsVerbs proves read-only publishing accepts inventory but
// rejects lifecycle verbs.
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
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "r3", Action: harnessproto.VerbRename, ID: "a1",
		Fields: map[string]string{"name": "x"},
	}); err != nil {
		t.Fatal(err)
	}
	res := readResult(t, oc)
	if res.OK || res.Error == "" {
		t.Fatalf("result = %+v, want a read-only rejection", res)
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
