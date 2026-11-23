package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
)

// fakeWaitDaemon scripts QueryWait answers per target and records calls.
type fakeWaitDaemon struct {
	mu      sync.Mutex
	answers map[string][]core.WaitResult
	calls   []string
}

func (f *fakeWaitDaemon) install(t *testing.T) {
	t.Helper()
	prev := waitQuery
	waitQuery = func(id, since string, timeout int) (core.WaitResult, error) {
		f.mu.Lock()
		f.calls = append(f.calls, id+"|"+since+"|"+strconv.Itoa(timeout))
		queue := f.answers[id]
		if len(queue) == 0 {
			f.mu.Unlock()
			// Nothing scripted: behave like a long-poll that never changes.
			time.Sleep(20 * time.Millisecond)
			return core.WaitResult{ID: id, Present: true, State: core.StateRunning, Token: "still"}, nil
		}
		res := queue[0]
		f.answers[id] = queue[1:]
		f.mu.Unlock()
		return res, nil
	}
	t.Cleanup(func() { waitQuery = prev })
}

func (f *fakeWaitDaemon) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// The CLI re-arms with the returned token after a timeout and returns on the
// first change.
func TestWaitOneRearmsWithToken(t *testing.T) {
	f := &fakeWaitDaemon{answers: map[string][]core.WaitResult{
		"m1": {
			{ID: "m1", Present: true, State: core.StateRunning, Token: "t1"},              // timed out
			{ID: "m1", Present: true, State: core.StateReady, Token: "t2", Changed: true}, // moved
		},
	}}
	f.install(t)
	res, err := waitOne(context.Background(), "m1", nil)
	if err != nil || res.State != core.StateReady {
		t.Fatalf("waitOne = %+v, %v", res, err)
	}
	calls := f.callLog()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "m1||") || !strings.HasPrefix(calls[1], "m1|t1|") {
		t.Fatalf("calls = %v", calls)
	}
}

// --until returns at once when the target is already there, and otherwise
// skips changes to other states.
func TestWaitOneUntil(t *testing.T) {
	f := &fakeWaitDaemon{answers: map[string][]core.WaitResult{
		"done": {{ID: "done", Present: true, State: core.StateReady, Token: "a"}},
		"busy": {
			{ID: "busy", Present: true, State: core.StateRunning, Token: "a"},
			{ID: "busy", Present: true, State: core.StateWaiting, Token: "b", Changed: true},
			{ID: "busy", Present: true, State: core.StateReady, Token: "c", Changed: true},
		},
	}}
	f.install(t)
	if res, err := waitOne(context.Background(), "done", []string{core.StateReady}); err != nil || res.Token != "a" {
		t.Fatalf("already-ready = %+v, %v", res, err)
	}
	res, err := waitOne(context.Background(), "busy", []string{core.StateReady, core.StateIdle})
	if err != nil || res.State != core.StateReady || res.Token != "c" {
		t.Fatalf("until ready = %+v, %v", res, err)
	}
	for _, c := range f.callLog() {
		if strings.HasPrefix(c, "busy|b|") {
			return // re-armed from the waiting state's token
		}
	}
	t.Fatalf("did not re-arm after a non-matching change: %v", f.callLog())
}

// Several targets: the first to change wins; an overall timeout with no change
// is an error.
func TestWaitAnyFirstChangeAndTimeout(t *testing.T) {
	f := &fakeWaitDaemon{answers: map[string][]core.WaitResult{
		"m2": {{ID: "m2", Present: true, State: core.StateWaiting, Token: "x", Changed: true}},
	}}
	f.install(t)
	res, err := waitAny(context.Background(), []string{"m1", "m2"}, nil)
	if err != nil || res.ID != "m2" {
		t.Fatalf("waitAny = %+v, %v", res, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := waitAny(ctx, []string{"m1"}, nil); !errors.Is(err, errWaitTimeout) {
		t.Fatalf("timeout err = %v", err)
	}
}

func TestCmdAgentWaitValidatesArgs(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"m1", "--until", "finished"},
		{"m1", "--timeout", "soon"},
		{"m1", "--bogus"},
	} {
		if err := cmdAgentWait(args); err == nil {
			t.Errorf("cmdAgentWait(%q) accepted", args)
		}
	}
}

func TestDescribeWait(t *testing.T) {
	if got := describeWait(core.WaitResult{ID: "m1"}); got != "m1\tgone" {
		t.Errorf("gone = %q", got)
	}
	got := describeWait(core.WaitResult{ID: "m1", Present: true, State: core.StateReady})
	if got != "m1\tready" {
		t.Errorf("ready = %q", got)
	}
}
