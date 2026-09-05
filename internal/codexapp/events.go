package codexapp

import (
	"context"
	"sync"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// events.go is the supervisor's live runtime-event fan-out. A structured session
// has no durable file to re-read on resubscribe (unlike the rollout tailer), so
// the hub assigns each emitted event a per-session monotonic seq and keeps a
// bounded ring of recent events. A subscriber (the provider's runtime-events
// pump, and any concurrent client) joins with an afterSeq cursor: it first
// receives the buffered events whose seq exceeds afterSeq, then the live stream —
// the same resume contract the on-disk tailer offers (docs/
// remote-provider-sessions.md §4), so the daemon wiring is drop-in.
//
// The seq space is owned by the supervisor for the App Server's lifetime. A
// daemon reconnect within that lifetime replays from the ring; a full supervisor
// restart begins a new seq space from 1, which a consumer dedups by seq exactly
// as it does for a tailer resync.

// eventRingCap bounds the replay ring. It trades memory for how far back a
// reconnecting subscriber can resume without a fresh full replay; the durable
// rollout remains the ultimate record, so this need not be unbounded.
const eventRingCap = 4096

type seqEvent struct {
	seq int64
	ev  harnessproto.RuntimeEvent
}

type subscriber struct {
	ch     chan harnessproto.RuntimeEventBatch
	cancel context.CancelFunc
}

// eventHub fans emitted events out to subscribers with per-session seq and a
// bounded replay ring. Safe for concurrent use.
type eventHub struct {
	mu     sync.Mutex
	seq    int64
	ring   []seqEvent // most recent eventRingCap events, in seq order
	subs   map[*subscriber]struct{}
	closed bool
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[*subscriber]struct{}{}}
}

// emit assigns the next seq to ev, records it in the ring, and delivers it to
// every subscriber as a single-event batch. A subscriber whose buffer is full is
// dropped (its channel closed): a stalled consumer must reconnect and replay from
// the ring rather than stall the read loop.
func (h *eventHub) emit(ev harnessproto.RuntimeEvent) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.seq++
	se := seqEvent{seq: h.seq, ev: ev}
	h.ring = append(h.ring, se)
	if len(h.ring) > eventRingCap {
		h.ring = h.ring[len(h.ring)-eventRingCap:]
	}
	batch := harnessproto.RuntimeEventBatch{Seq: se.seq, Runtime: harnessproto.RuntimeCodex, Events: []harnessproto.RuntimeEvent{ev}}
	var dead []*subscriber
	for s := range h.subs {
		select {
		case s.ch <- batch:
		default:
			dead = append(dead, s)
		}
	}
	for _, s := range dead {
		h.removeLocked(s)
	}
	h.mu.Unlock()
}

// subscribe joins a subscriber at afterSeq: the returned channel first carries a
// replay batch of every buffered event with seq > afterSeq (when any remain in
// the ring), then live single-event batches until ctx is cancelled or the hub is
// closed. A caller that has fallen entirely off the ring (afterSeq below the
// oldest buffered seq) still gets everything the ring holds — never a gap
// silently skipped past the buffer, but never a promise of pre-ring history.
func (h *eventHub) subscribe(ctx context.Context, afterSeq int64) <-chan harnessproto.RuntimeEventBatch {
	ctx, cancel := context.WithCancel(ctx)
	sub := &subscriber{ch: make(chan harnessproto.RuntimeEventBatch, 64), cancel: cancel}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		close(sub.ch)
		return sub.ch
	}
	var replay []harnessproto.RuntimeEvent
	var replaySeq int64
	for _, se := range h.ring {
		if se.seq > afterSeq {
			replay = append(replay, se.ev)
			replaySeq = se.seq
		}
	}
	h.subs[sub] = struct{}{}
	h.mu.Unlock()

	out := make(chan harnessproto.RuntimeEventBatch, cap(sub.ch)+1)
	go h.pump(ctx, sub, out, replay, replaySeq)
	return out
}

// pump forwards the initial replay (if any) then the subscriber's live batches to
// out, removing the subscriber and closing out when ctx ends or the hub closes
// the channel.
func (h *eventHub) pump(ctx context.Context, sub *subscriber, out chan<- harnessproto.RuntimeEventBatch, replay []harnessproto.RuntimeEvent, replaySeq int64) {
	defer close(out)
	defer h.remove(sub)

	if len(replay) > 0 {
		select {
		case out <- harnessproto.RuntimeEventBatch{Seq: replaySeq, Runtime: harnessproto.RuntimeCodex, Events: replay}:
		case <-ctx.Done():
			return
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-sub.ch:
			if !ok {
				return
			}
			select {
			case out <- b:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (h *eventHub) remove(sub *subscriber) {
	h.mu.Lock()
	h.removeLocked(sub)
	h.mu.Unlock()
}

// removeLocked drops a subscriber and closes its channel. Caller holds h.mu.
func (h *eventHub) removeLocked(sub *subscriber) {
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	sub.cancel()
	close(sub.ch)
}

// close tears down every subscriber and blocks further emits. Idempotent.
func (h *eventHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for s := range h.subs {
		s.cancel()
		close(s.ch)
		delete(h.subs, s)
	}
}

// lastSeq reports the highest seq emitted so far (introspection / tests).
func (h *eventHub) lastSeq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}
