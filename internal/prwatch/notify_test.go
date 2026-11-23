package prwatch

import (
	"context"
	"sync"
	"testing"
	"time"
)

// notifyHarness runs a NoPoll watcher and records every Deliver call.
type notifyHarness struct {
	w       *Watcher
	mu      sync.Mutex
	calls   [][]Wake
	verdict Delivery
	called  chan struct{}
}

func newNotifyHarness(t *testing.T, window time.Duration) *notifyHarness {
	t.Helper()
	h := &notifyHarness{verdict: Delivered, called: make(chan struct{}, 16)}
	h.w = New(Config{
		Deliver: func(_ context.Context, _ string, wakes []Wake) []Delivery {
			h.mu.Lock()
			h.calls = append(h.calls, append([]Wake(nil), wakes...))
			v := h.verdict
			h.mu.Unlock()
			out := make([]Delivery, len(wakes))
			for i := range out {
				out[i] = v
			}
			h.called <- struct{}{}
			return out
		},
		Logf:   t.Logf,
		Window: window,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

func (h *notifyHarness) waitCall(t *testing.T) []Wake {
	t.Helper()
	select {
	case <-h.called:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls[len(h.calls)-1]
}

func (h *notifyHarness) noCall(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-h.called:
		t.Fatal("unexpected delivery")
	case <-time.After(within):
	}
}

// A burst of member changes inside the window reaches the coordinator as one
// batch with one line per member, the latest state winning — and with GitHub
// polling off.
func TestNotifyCoalescesBurstPerMember(t *testing.T) {
	h := newNotifyHarness(t, 50*time.Millisecond)
	h.w.Notify(Wake{Kind: KindMember, SessionID: "m1", Workgroup: "wg", State: "waiting", Text: "m1 waiting"})
	h.w.Notify(Wake{Kind: KindMember, SessionID: "m2", Workgroup: "wg", State: "ready", Text: "m2 finished"})
	h.w.Notify(Wake{Kind: KindMember, SessionID: "m1", Workgroup: "wg", State: "ready", Text: "m1 finished"})
	got := h.waitCall(t)
	if len(got) != 2 || got[0].Text != "m1 finished" || got[1].Text != "m2 finished" {
		t.Fatalf("batch = %+v", got)
	}
	h.w.Kick()
	h.noCall(t, 100*time.Millisecond) // delivered: nothing left to offer
}

// A wake the coordinator could not take is retried when the daemon kicks the
// queue (the coordinator's own state moved), not only at FlushInterval.
func TestKickRetriesDeferredWake(t *testing.T) {
	h := newNotifyHarness(t, 10*time.Millisecond)
	h.mu.Lock()
	h.verdict = Deferred
	h.mu.Unlock()
	h.w.Notify(Wake{Kind: KindMember, SessionID: "m1", Workgroup: "wg", State: "ready", Text: "m1 finished"})
	h.waitCall(t) // deferred: the coordinator is mid-turn
	h.mu.Lock()
	h.verdict = Delivered
	h.mu.Unlock()
	h.w.Kick()
	if got := h.waitCall(t); len(got) != 1 || got[0].SessionID != "m1" {
		t.Fatalf("retry batch = %+v", got)
	}
	h.w.Kick()
	h.noCall(t, 50*time.Millisecond) // nothing left to offer
}

// Queued steers are separate messages: two different ones to the same session
// both stay queued, in order, and only an identical resend collapses.
func TestNotifyKeepsEachQueuedSteer(t *testing.T) {
	h := newNotifyHarness(t, 50*time.Millisecond)
	h.w.Notify(Wake{Kind: KindSteer, SessionID: "a1", Workgroup: "steer:a1", State: "interject", Text: "first"})
	h.w.Notify(Wake{Kind: KindSteer, SessionID: "a1", Workgroup: "steer:a1", State: "interject", Text: "second"})
	h.w.Notify(Wake{Kind: KindSteer, SessionID: "a1", Workgroup: "steer:a1", State: "interject", Text: "first"})
	got := h.waitCall(t)
	if len(got) != 2 || got[0].Text != "first" || got[1].Text != "second" {
		t.Fatalf("batch = %+v, want first then second", got)
	}
}

// Notify ignores a wake with no route.
func TestNotifyIgnoresUnroutedWake(t *testing.T) {
	h := newNotifyHarness(t, 10*time.Millisecond)
	h.w.Notify(Wake{Kind: KindMember, SessionID: "m1"})
	h.noCall(t, 50*time.Millisecond)
}
