package sessionrpc

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A long-poll on the parallel lane blocks by design; neither the session's
// fast calls nor its slow-lane actions may queue behind it, and Close cancels
// it rather than waiting it out.
func TestParallelCallDoesNotHoldEitherLane(t *testing.T) {
	waiting := make(chan struct{}, MaxParallelRequests)
	cancelled := make(chan struct{}, MaxParallelRequests)
	f := newFixture(t, Callbacks{
		Dispatch: func(ctx context.Context, request DispatchRequest) (DispatchResult, error) {
			if request.Call.Verb == "wait" {
				waiting <- struct{}{}
				<-ctx.Done()
				cancelled <- struct{}{}
				return DispatchResult{Status: StatusOK, Body: []byte("cancelled")}, nil
			}
			return DispatchResult{Status: StatusOK, Body: []byte("ok")}, nil
		},
	}, ServerOptions{
		Slow:     func(call Call) bool { return call.Verb == "slow" },
		Parallel: func(call Call) bool { return call.Verb == "wait" },
	})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.server.Run(ctx) }()
	defer func() { cancel(); <-served }()

	// Two concurrent long-polls from the same session.
	var polls sync.WaitGroup
	defer polls.Wait() // before the fixture closes their clients
	for i := 0; i < 2; i++ {
		client := f.client(clientOptions{})
		polls.Add(1)
		go func() {
			defer polls.Done()
			qctx, qcancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer qcancel()
			_, _ = client.Query(qctx, Query{Verb: "wait"})
		}()
		select {
		case <-waiting:
		case <-time.After(10 * time.Second):
			t.Fatalf("parallel call %d never dispatched", i)
		}
	}
	if !f.server.Busy() {
		t.Fatal("server reports idle while parallel calls dispatch")
	}

	client := f.client(clientOptions{})
	for _, call := range []func(context.Context) (Result, error){
		func(c context.Context) (Result, error) { return client.Query(c, Query{Verb: "version"}) },
		func(c context.Context) (Result, error) {
			return client.Action(c, Action{Verb: "slow", ID: "subject-a"})
		},
	} {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := call(cctx)
		ccancel()
		if err != nil || result.Status != StatusOK {
			t.Fatalf("call behind running long-polls = %+v, %v", result, err)
		}
	}

	closed := make(chan error, 1)
	go func() { closed <- f.server.Close() }()
	for i := 0; i < 2; i++ {
		select {
		case <-cancelled:
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not cancel a parallel call")
		}
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after cancelling parallel calls")
	}
}
