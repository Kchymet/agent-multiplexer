package provider

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// fakeStream is a controllable RuntimeEventStream: it records the (sessionID,
// afterSeq) it was called with and returns a channel the test feeds. ok reports
// whether a record exists for the session.
type fakeStream struct {
	mu       sync.Mutex
	ch       chan harnessproto.RuntimeEventBatch
	ok       bool
	gotSess  string
	gotAfter int64
	calls    int
}

func (f *fakeStream) stream(_ context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotSess, f.gotAfter = sessionID, afterSeq
	f.calls++
	return f.ch, f.ok
}

func (f *fakeStream) observed() (sessionID string, afterSeq int64, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotSess, f.gotAfter, f.calls
}

func (f *fakeStream) replaceChannel(ch chan harnessproto.RuntimeEventBatch) {
	f.mu.Lock()
	f.ch = ch
	f.mu.Unlock()
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
	src.set([]core.Session{{ID: "sess-1"}})
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
	subscribe(t, oc)
	readSessions(t, oc)

	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1", AfterSeq: 3,
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Feed a batch; it must arrive as a runtime-events frame.
	fs.ch <- harnessproto.RuntimeEventBatch{
		Seq:     5,
		Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{
			{Type: "text", ItemID: "m1", Direction: "out", Payload: json.RawMessage(`{"text":"hi","final":true}`)},
		},
	}
	m := readRuntimeEvents(t, oc)
	if m.SessionID != "sess-1" || m.Seq != 5 || len(m.Events) != 1 || m.Events[0].Type != "text" {
		t.Fatalf("runtime-events frame = %+v", m)
	}
	// The batch's runtime rides on the frame, so the orchestrator never has to
	// assume one.
	if m.Runtime != harnessproto.RuntimeCodex {
		t.Fatalf("frame runtime = %q, want codex", m.Runtime)
	}
	gotSess, gotAfter, calls := fs.observed()
	if gotSess != "sess-1" || gotAfter != 3 || calls != 1 {
		t.Fatalf("stream called %d time(s) with (%q,%d), want once with (sess-1,3)", calls, gotSess, gotAfter)
	}
}

// TestRuntimeEventsNoRecordEmitsNothing proves a session with no structured
// record (ok=false) produces no frame — honest degradation, feature still
// advertised.
func TestRuntimeEventsNoRecordEmitsNothing(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-x"}})
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
	subscribe(t, oc)
	readSessions(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-x",
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// A session action result proves the connection is alive and no runtime-events
	// frame jumped ahead.
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "probe", Action: harnessproto.VerbRename, ID: "sess-x",
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFrame(t, oc); got.Type != harnessproto.HSessionResult {
		t.Fatalf("expected session-result frame, got %q (runtime-events should emit nothing)", got.Type)
	}
}

// TestRuntimeEventsRequireCurrentPublishedOpaqueID proves caller-chosen runtime
// IDs never reach the resolver unless they are exact members of the current
// published set, and path/URI aliases are rejected even if guessed.
func TestRuntimeEventsRequireCurrentPublishedOpaqueID(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
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
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	for _, id := range []string{"", "unknown", "../sess-1", "/tmp/transcript", `sibling\\record`, "file://sess-1"} {
		if err := oc.WriteMux(harnessproto.MuxMsg{
			Type: harnessproto.MRuntimeEventsSubscribe, SessionID: id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1", AfterSeq: 7,
	}); err != nil {
		t.Fatal(err)
	}
	fs.ch <- harnessproto.RuntimeEventBatch{
		Seq: 8, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"ok"}`)}},
	}
	readRuntimeEvents(t, oc)
	got, after, calls := fs.observed()
	if calls != 1 || got != "sess-1" || after != 7 {
		t.Fatalf("resolver calls=%d last=(%q,%d), want one exact published ID", calls, got, after)
	}
}

// TestRuntimeEventsRemovalIsWriteBarrier proves removal cancels a live pump
// before the reduced snapshot is sent. A batch queued afterward cannot cross
// the barrier; the next frame is the explicit denial for a targeted action.
func TestRuntimeEventsRemovalIsWriteBarrier(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 2), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
		RuntimeEvents: true, RuntimeEventStream: fs.stream,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	fs.ch <- harnessproto.RuntimeEventBatch{
		Seq: 1, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"first"}`)}},
	}
	readRuntimeEvents(t, oc)

	src.set([]core.Session{})
	if reduced := readSessions(t, oc); len(reduced.Sessions) != 0 {
		t.Fatalf("reduced snapshot = %+v, want empty", reduced.Sessions)
	}
	fs.ch <- harnessproto.RuntimeEventBatch{
		Seq: 2, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"revoked"}`)}},
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "after-revoke",
		Action: harnessproto.VerbRename, ID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	got := readFrame(t, oc)
	if got.Type != harnessproto.HSessionResult || got.OK || got.Error != errSessionNotPublished.Error() {
		t.Fatalf("first post-revoke frame = %+v, want unpublished session-result", got)
	}

	// Revocation deletes the old subscription marker. Republishing the same
	// opaque ID can therefore establish a distinct fresh stream; the old pump's
	// deferred cleanup must not erase this replacement subscription.
	fresh := make(chan harnessproto.RuntimeEventBatch, 1)
	fs.replaceChannel(fresh)
	publishRows(t, oc, src, core.Session{ID: "sess-1"})
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1", AfterSeq: 2,
	}); err != nil {
		t.Fatal(err)
	}
	fresh <- harnessproto.RuntimeEventBatch{
		Seq: 3, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"fresh"}`)}},
	}
	if freshFrame := readRuntimeEvents(t, oc); freshFrame.Seq != 3 {
		t.Fatalf("fresh subscription frame = %+v, want seq 3", freshFrame)
	}
	_, after, calls := fs.observed()
	if after != 2 || calls != 2 {
		t.Fatalf("fresh resolver calls=%d after=%d, want calls=2 after=2", calls, after)
	}
}

// TestRuntimeResolverRemovalCancelsBlockingLookup covers the admission half of
// the runtime barrier. A resolver that accepted a published ID but stopped
// replying is cancelled and joined before the reduced snapshot is emitted.
func TestRuntimeResolverRemovalCancelsBlockingLookup(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
		RuntimeEvents: true,
		RuntimeEventStream: func(ctx context.Context, _ string, _ int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, false
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime resolver did not start")
	}
	src.set([]core.Session{})
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("removal did not cancel blocking runtime resolver")
	}
	if reduced := readSessions(t, oc); len(reduced.Sessions) != 0 {
		t.Fatalf("reduced snapshot = %+v, want empty", reduced.Sessions)
	}
}

func TestRetainedRuntimeOpeningDoesNotBlockSiblingRemoval(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}, {ID: "sess-2"}})
	started := make(chan struct{})
	release := make(chan struct{})
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
		RuntimeEvents: true,
		RuntimeEventStream: func(ctx context.Context, _ string, _ int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
			close(started)
			select {
			case <-release:
				return make(chan harnessproto.RuntimeEventBatch), true
			case <-ctx.Done():
				return nil, false
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	src.set([]core.Session{{ID: "sess-1"}})
	if reduced := readSessions(t, oc); len(reduced.Sessions) != 1 || reduced.Sessions[0].ID != "sess-1" {
		t.Fatalf("reduced snapshot = %+v, want retained sess-1", reduced.Sessions)
	}
	close(release)
}

func TestRetainedRuntimeStreamSurvivesUnrelatedSnapshotChange(t *testing.T) {
	conns := make(chan net.Conn, 1)
	src := &mutableSource{}
	src.set([]core.Session{{ID: "sess-1"}, {ID: "sess-2", Title: "old"}})
	fs := &fakeStream{ch: make(chan harnessproto.RuntimeEventBatch, 2), ok: true}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, SessionPollInterval: 5 * time.Millisecond,
		RuntimeEvents: true, RuntimeEventStream: fs.stream,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	subscribe(t, oc)
	readSessions(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRuntimeEventsSubscribe, SessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	fs.ch <- harnessproto.RuntimeEventBatch{Seq: 1, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"before"}`)}}}
	readRuntimeEvents(t, oc)

	src.set([]core.Session{{ID: "sess-1"}, {ID: "sess-2", Title: "new"}})
	readSessions(t, oc)
	fs.ch <- harnessproto.RuntimeEventBatch{Seq: 2, Runtime: harnessproto.RuntimeCodex,
		Events: []harnessproto.RuntimeEvent{{Type: "notice", Payload: json.RawMessage(`{"text":"after"}`)}}}
	if got := readRuntimeEvents(t, oc); got.Seq != 2 {
		t.Fatalf("retained stream frame = %+v, want seq 2 without re-subscribe", got)
	}
	if _, _, calls := fs.observed(); calls != 1 {
		t.Fatalf("resolver calls = %d, want one retained subscription", calls)
	}
}
