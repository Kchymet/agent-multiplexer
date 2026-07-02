package provider

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"amux/internal/harnessproto"
)

// fakeStream is a controllable RuntimeEventStream: it records the (sessionID,
// afterSeq) it was called with and returns a channel the test feeds. ok reports
// whether a record exists for the session.
type fakeStream struct {
	ch       chan harnessproto.RuntimeEventBatch
	ok       bool
	gotSess  string
	gotAfter int64
}

func (f *fakeStream) stream(_ context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
	f.gotSess, f.gotAfter = sessionID, afterSeq
	return f.ch, f.ok
}

func readRuntimeEvents(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m := readFrame(t, oc)
	if m.Type != harnessproto.HRuntimeEvents {
		t.Fatalf("frame = %q, want runtime-events", m.Type)
	}
	return m
}

// TestRuntimeEventsAdvertisedWithSessions proves "runtime-events" is advertised
// only when opted in alongside published sessions, and rides next to "sessions".
func TestRuntimeEventsAdvertisedWithSessions(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 1), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll,
		RuntimeEvents: true, RuntimeEventStream: fs.stream,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if !hasFeature(reg, harnessproto.RuntimeEventsFeature) {
		t.Fatalf("features = %v, want runtime-events", reg.Capabilities.Features)
	}
	if !hasFeature(reg, harnessproto.SessionsFeature) {
		t.Fatalf("runtime-events must ride alongside sessions: %v", reg.Capabilities.Features)
	}
}

// TestRuntimeEventsRequiresSessions proves the feature stays inactive when
// PublishSessions is off (it streams events for *published* sessions).
func TestRuntimeEventsRequiresSessions(t *testing.T) {
	conns := make(chan net.Conn, 1)
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 1), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		RuntimeEvents: true, RuntimeEventStream: fs.stream, // no PublishSessions
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if hasFeature(reg, harnessproto.RuntimeEventsFeature) {
		t.Fatalf("advertised runtime-events without published sessions: %v", reg.Capabilities.Features)
	}
}

// TestRuntimeEventsSubscribeStreamsFrames drives a subscribe and asserts the
// batch is forwarded as a runtime-events frame carrying the sessionId and seq.
func TestRuntimeEventsSubscribeStreamsFrames(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 2), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll,
		RuntimeEvents: true, RuntimeEventStream: fs.stream,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)

	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1", AfterSeq: 3,
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Feed a batch; it must arrive as a runtime-events frame.
	fs.ch <- harnessproto.RuntimeEventBatch{
		Seq: 5,
		Events: []harnessproto.RuntimeEvent{
			{Type: "text", ItemID: "m1", Direction: "out", Payload: json.RawMessage(`{"text":"hi","final":true}`)},
		},
	}
	m := readRuntimeEvents(t, oc)
	if m.SessionID != "sess-1" || m.Seq != 5 || len(m.Events) != 1 || m.Events[0].Type != "text" {
		t.Fatalf("runtime-events frame = %+v", m)
	}
	if fs.gotSess != "sess-1" || fs.gotAfter != 3 {
		t.Fatalf("stream called with (%q,%d), want (sess-1,3)", fs.gotSess, fs.gotAfter)
	}
}

// TestRuntimeEventsNoRecordEmitsNothing proves a session with no structured
// record (ok=false) produces no frame — honest degradation, feature still
// advertised.
func TestRuntimeEventsNoRecordEmitsNothing(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch), ok: false}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll,
		RuntimeEvents: true, RuntimeEventStream: fs.stream,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-x",
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// A subsequent sessions-subscribe → sessions frame proves the connection is
	// alive and no runtime-events frame jumped ahead.
	subscribe(t, oc)
	if got := readFrame(t, oc); got.Type != harnessproto.HSessions {
		t.Fatalf("expected sessions frame, got %q (a runtime-events frame should not appear)", got.Type)
	}
}
