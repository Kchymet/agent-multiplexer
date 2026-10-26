package sessionrpc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowLaneFixture serves one mailbox whose "slow" action blocks until release
// is closed. order records dispatches in the order they started.
type slowLaneFixture struct {
	*fixture
	release chan struct{}
	started chan string
	running atomic.Int32
	peak    atomic.Int32
	mu      sync.Mutex
	order   []string
}

func newSlowLaneFixture(t *testing.T) *slowLaneFixture {
	t.Helper()
	sf := &slowLaneFixture{release: make(chan struct{}), started: make(chan string, 64)}
	sf.fixture = newFixture(t, Callbacks{
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			if request.Call.Verb == "slow" {
				now := sf.running.Add(1)
				for peak := sf.peak.Load(); now > peak && !sf.peak.CompareAndSwap(peak, now); peak = sf.peak.Load() {
				}
				sf.mu.Lock()
				sf.order = append(sf.order, request.Call.Fields["n"])
				sf.mu.Unlock()
				sf.started <- request.Call.Fields["n"]
				<-sf.release
				sf.running.Add(-1)
			}
			return DispatchResult{Status: StatusOK, Body: []byte("ok")}, nil
		},
	}, ServerOptions{Slow: func(call Call) bool { return call.Verb == "slow" }})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- sf.server.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-sf.release:
		default:
			close(sf.release)
		}
		cancel()
		<-served
	})
	return sf
}

func (sf *slowLaneFixture) slow(t *testing.T, n string) <-chan error {
	t.Helper()
	client := sf.client(clientOptions{})
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := client.Action(ctx, Action{Verb: "slow", ID: "subject-a", Fields: map[string]string{"n": n}})
		done <- err
	}()
	return done
}

func (sf *slowLaneFixture) awaitStart(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-sf.started:
		if got != want {
			t.Fatalf("slow call %s started, want %s", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("slow call %s never started", want)
	}
}

func TestFastCallAnsweredWhileSlowCallRunsInSameMailbox(t *testing.T) {
	sf := newSlowLaneFixture(t)
	slowDone := sf.slow(t, "1")
	sf.awaitStart(t, "1")

	client := sf.client(clientOptions{})
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		result, err := client.Query(ctx, Query{Verb: "version"})
		cancel()
		if err != nil || result.Status != StatusOK {
			t.Fatalf("fast call %d behind a running slow call = %+v, %v", i, result, err)
		}
		t.Logf("fast call %d answered in %v while the slow call ran", i, time.Since(start))
	}
	if !sf.server.Busy() {
		t.Fatal("server reports an idle slow lane while a slow call dispatches")
	}
	close(sf.release)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow call: %v", err)
	}
}

// One worker per mailbox: slow calls dispatch one at a time in arrival order,
// and every one queued beyond the lane's bound is still served.
func TestSlowLaneKeepsArrivalOrderAndDrains(t *testing.T) {
	sf := newSlowLaneFixture(t)
	first := sf.slow(t, "0")
	sf.awaitStart(t, "0")
	var rest []<-chan error
	for i := 1; i <= MaxSlowRequests+2; i++ {
		// Publish each only once the previous one is claimed, so arrival order
		// is unambiguous (request ids order by millisecond only).
		rest = append(rest, sf.slow(t, string(rune('a'+i))))
		sf.awaitClaimed(t, i+1)
		time.Sleep(2 * time.Millisecond) // sign the next one in a later millisecond
	}
	close(sf.release)
	for _, done := range append([]<-chan error{first}, rest...) {
		if err := <-done; err != nil {
			t.Fatalf("slow call: %v", err)
		}
	}
	if peak := sf.peak.Load(); peak != 1 {
		t.Fatalf("%d slow calls dispatched at once; the lane is one worker", peak)
	}
	sf.mu.Lock()
	defer sf.mu.Unlock()
	for i := 1; i < len(sf.order); i++ {
		if sf.order[i] < sf.order[i-1] {
			t.Fatalf("slow calls dispatched out of arrival order: %v", sf.order)
		}
	}
}

// awaitClaimed waits until n requests sit claimed in processing/.
func (sf *slowLaneFixture) awaitClaimed(t *testing.T, n int) {
	t.Helper()
	dir := filepath.Join(sf.grant.MailboxHostDir, processingDirName)
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		claimed := 0
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".req") {
				claimed++
			}
		}
		if claimed >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests claimed, want %d", claimed, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCloseJoinsSlowLane(t *testing.T) {
	sf := newSlowLaneFixture(t)
	slowDone := sf.slow(t, "1")
	sf.awaitStart(t, "1")
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = sf.server.Close()
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while the slow lane was dispatching")
	case <-time.After(100 * time.Millisecond):
	}
	close(sf.release)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close never joined the slow lane")
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow call: %v", err)
	}
}
