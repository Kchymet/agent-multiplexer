package provider

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

type historyCall struct {
	sessionID string
	before    int64
	limit     int
}

func readPage(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m := readFrame(t, oc)
	if m.Type != harnessproto.HRuntimeEventsPage {
		t.Fatalf("frame = %q, want runtime-events-page", m.Type)
	}
	return m
}

func startHistoryProvider(t *testing.T, src *mutableSource, history func(context.Context, string, int64, int) (harnessproto.RuntimeEventHistory, bool)) *harnessproto.Conn {
	t.Helper()
	conns := make(chan net.Conn, 1)
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 1), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
		RuntimeEvents: true, RuntimeEventStream: fs.stream, RuntimeEventHistory: history,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.Run(ctx)
	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	return oc
}

func TestRuntimeHistoryAdvertisedOnlyWithSource(t *testing.T) {
	history := func(context.Context, string, int64, int) (harnessproto.RuntimeEventHistory, bool) {
		return harnessproto.RuntimeEventHistory{}, false
	}
	for _, c := range []struct {
		name    string
		history func(context.Context, string, int64, int) (harnessproto.RuntimeEventHistory, bool)
		events  bool
		want    bool
	}{
		{"with source", history, true, true},
		{"no source", nil, true, false},
		{"no runtime-events", history, false, false},
	} {
		conns := make(chan net.Conn, 1)
		src := &mutableSource{}
		fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 1), ok: true}
		p := newFast(Config{
			Orchestrator: "pipe", Dial: pipeDialer(conns),
			PublishSessions: true, Sessions: src.poll,
			RuntimeEvents: c.events, RuntimeEventStream: fs.stream, RuntimeEventHistory: c.history,
		})
		ctx, cancel := context.WithCancel(context.Background())
		go p.Run(ctx)
		reg := expectRegister(t, harnessproto.NewConn(<-conns))
		cancel()
		if got := hasFeature(reg, harnessproto.RuntimeEventsHistoryFeature); got != c.want {
			t.Fatalf("%s: features = %v, want history advertised=%v", c.name, reg.Capabilities.Features, c.want)
		}
	}
}

func TestRuntimeHistoryAnswersPublishedSession(t *testing.T) {
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
	calls := make(chan historyCall, 1)
	oc := startHistoryProvider(t, src, func(_ context.Context, id string, before int64, limit int) (harnessproto.RuntimeEventHistory, bool) {
		calls <- historyCall{id, before, limit}
		return harnessproto.RuntimeEventHistory{
			Seq: 40, Head: 57, Runtime: harnessproto.RuntimeCodex, HasMore: true,
			Events: []harnessproto.RuntimeEvent{
				{Type: "text", Payload: json.RawMessage(`{"text":"a"}`)},
				{Type: "text", Payload: json.RawMessage(`{"text":"b"}`)},
			},
		}, true
	})

	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsHistory, ReqID: "h1", SessionID: "sess-1", BeforeSeq: 41, Limit: 2,
	}); err != nil {
		t.Fatal(err)
	}
	m := readPage(t, oc)
	if !m.OK || m.ReqID != "h1" || m.SessionID != "sess-1" || m.Seq != 40 || m.HeadSeq != 57 ||
		!m.HasMore || m.Runtime != harnessproto.RuntimeCodex || len(m.Events) != 2 {
		t.Fatalf("page = %+v", m)
	}
	if got := <-calls; got != (historyCall{"sess-1", 41, 2}) {
		t.Fatalf("history called with %+v", got)
	}
}

func TestRuntimeHistoryRefusesUnpublishedAndMalformed(t *testing.T) {
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
	oc := startHistoryProvider(t, src, func(context.Context, string, int64, int) (harnessproto.RuntimeEventHistory, bool) {
		t.Error("history source reached for a refused request")
		return harnessproto.RuntimeEventHistory{}, false
	})
	for _, c := range []struct {
		msg  harnessproto.MuxMsg
		want string
	}{
		{harnessproto.MuxMsg{SessionID: "other"}, errSessionNotPublished.Error()},
		{harnessproto.MuxMsg{SessionID: "../sess-1"}, "invalid request"},
		{harnessproto.MuxMsg{SessionID: "sess-1", BeforeSeq: -1}, "invalid request"},
	} {
		c.msg.Type, c.msg.ReqID = harnessproto.MRuntimeEventsHistory, "r"
		if err := oc.WriteMux(c.msg); err != nil {
			t.Fatal(err)
		}
		if m := readPage(t, oc); m.OK || m.Error != c.want || len(m.Events) != 0 {
			t.Fatalf("%+v: page = %+v, want error %q", c.msg, m, c.want)
		}
	}
}

// Removing a session while its page is being read cancels the read, and the
// reply that follows carries no events.
func TestRuntimeHistoryRemovalCancelsRead(t *testing.T) {
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
	started := make(chan struct{})
	oc := startHistoryProvider(t, src, func(ctx context.Context, _ string, _ int64, _ int) (harnessproto.RuntimeEventHistory, bool) {
		close(started)
		<-ctx.Done()
		return harnessproto.RuntimeEventHistory{Seq: 1, Head: 1, Events: []harnessproto.RuntimeEvent{{Type: "text"}}}, true
	})
	if err := oc.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MRuntimeEventsHistory, ReqID: "h", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	<-started
	src.set([]core.Session{})
	var page, sessions bool
	for !page || !sessions {
		m := readFrame(t, oc)
		switch m.Type {
		case harnessproto.HSessions:
			sessions = true
		case harnessproto.HRuntimeEventsPage:
			page = true
			if m.OK || len(m.Events) != 0 || m.Error != errSessionNotPublished.Error() {
				t.Fatalf("page after removal = %+v", m)
			}
		}
	}
}
