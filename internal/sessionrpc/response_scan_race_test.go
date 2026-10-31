package sessionrpc

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The fast lane reserves response capacity while the slow lane publishes, and
// both scan responses/ through one shared directory stream. Unserialized, that
// corrupts os.File's directory state: `go test -race` reports it, and a live
// daemon crashed in os.(*File).readdir. Run with -race.
func TestFastReservationAndSlowPublicationShareResponseScan(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{Slow: func(call Call) bool { return call.Verb == "slow" }})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.server.Run(ctx) }()
	defer func() {
		cancel()
		<-served
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	var ok [2]atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		lane := i % 2
		verb := [2]string{"fast", "slow"}[lane]
		client := f.client(clientOptions{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				callCtx, callCancel := context.WithTimeout(context.Background(), 2*time.Second)
				if _, err := client.Query(callCtx, Query{Verb: verb}); err == nil {
					ok[lane].Add(1)
				}
				callCancel()
			}
		}()
	}
	wg.Wait()
	if ok[0].Load() == 0 || ok[1].Load() == 0 {
		t.Fatalf("both lanes must complete calls: fast=%d slow=%d", ok[0].Load(), ok[1].Load())
	}
}
