package codexapp

import (
	"context"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func ev(typ string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: typ, Payload: []byte(`{}`)}
}

func TestHubAssignsMonotonicSeq(t *testing.T) {
	h := newEventHub()
	defer h.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx, 0)

	h.emit(ev(harnessproto.TypeText))
	h.emit(ev(harnessproto.TypeText))

	b1 := recvBatch(t, ch)
	b2 := recvBatch(t, ch)
	if b1.Seq != 1 || b2.Seq != 2 {
		t.Fatalf("seqs = %d,%d", b1.Seq, b2.Seq)
	}
	if b1.Runtime != harnessproto.RuntimeCodex {
		t.Fatalf("batch runtime = %q", b1.Runtime)
	}
}

func TestHubReplaysAfterSeq(t *testing.T) {
	h := newEventHub()
	defer h.close()
	// Emit three before anyone subscribes; they land in the ring.
	h.emit(ev(harnessproto.TypeText))
	h.emit(ev(harnessproto.TypeText))
	h.emit(ev(harnessproto.TypeThinking))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx, 1) // resume after seq 1 → replay seqs 2,3

	b := recvBatch(t, ch)
	if b.Seq != 3 || len(b.Events) != 2 {
		t.Fatalf("replay batch seq=%d len=%d", b.Seq, len(b.Events))
	}
	if b.Events[0].Type != harnessproto.TypeText || b.Events[1].Type != harnessproto.TypeThinking {
		t.Fatalf("replay events = %+v", b.Events)
	}
}

func TestHubReplayThenLive(t *testing.T) {
	h := newEventHub()
	defer h.close()
	h.emit(ev(harnessproto.TypeText)) // seq 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx, 0)
	first := recvBatch(t, ch) // replay of seq 1
	if first.Seq != 1 {
		t.Fatalf("replay seq = %d", first.Seq)
	}
	h.emit(ev(harnessproto.TypeThinking)) // seq 2 live
	live := recvBatch(t, ch)
	if live.Seq != 2 {
		t.Fatalf("live seq = %d", live.Seq)
	}
}

func TestHubCloseEndsSubscribers(t *testing.T) {
	h := newEventHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := h.subscribe(ctx, 0)
	h.close()
	select {
	case _, ok := <-ch:
		if ok {
			// May receive nothing; the channel must eventually close.
			for range ch { // drain
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel not closed after hub.close")
	}
}

func TestHubEmitAfterCloseNoPanic(t *testing.T) {
	h := newEventHub()
	h.close()
	h.emit(ev(harnessproto.TypeText)) // must be a no-op, not a panic
	if h.lastSeq() != 0 {
		t.Fatalf("emit after close advanced seq to %d", h.lastSeq())
	}
}

func recvBatch(t *testing.T, ch <-chan harnessproto.RuntimeEventBatch) harnessproto.RuntimeEventBatch {
	t.Helper()
	select {
	case b, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a batch")
		return harnessproto.RuntimeEventBatch{}
	}
}
