package sessionrpc

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMeasureRoundTripLatency reports request→response latency for a trivial
// query through the production Client and Server.Run, idle and with other
// eight clients (about 400 calls/s) calling the same mailbox. The server polls at the daemon's mailbox
// interval. Opt-in: AMUX_MEASURE_RPC_LATENCY=1.
func TestMeasureRoundTripLatency(t *testing.T) {
	if os.Getenv("AMUX_MEASURE_RPC_LATENCY") == "" {
		t.Skip("set AMUX_MEASURE_RPC_LATENCY=1 to measure")
	}
	for _, load := range []int{0, 8} {
		f := newFixture(t, Callbacks{}, ServerOptions{PollInterval: 50 * time.Millisecond})
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan error, 1)
		go func() { served <- f.server.Run(ctx) }()
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			releaseReadResponses(ctx, filepath.Join(f.grant.MailboxHostDir, ResponsesDirName))
		}()
		for i := 0; i < load; i++ {
			client := f.client(clientOptions{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				// ~50 calls/s each: busy hooks, kept under the retention cap.
				for ctx.Err() == nil {
					_, _ = client.Query(ctx, Query{Verb: "version"})
					time.Sleep(20 * time.Millisecond)
				}
			}()
		}
		probe := f.client(clientOptions{})
		var samples []time.Duration
		for i := 0; i < 200; i++ {
			start := time.Now()
			callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := probe.Query(callCtx, Query{Verb: "version"})
			callCancel()
			if err != nil || result.Status != StatusOK {
				t.Fatalf("probe %d = %+v, %v", i, result, err)
			}
			samples = append(samples, time.Since(start))
		}
		cancel()
		wg.Wait()
		<-served
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		pct := func(p int) time.Duration { return samples[len(samples)*p/100] }
		t.Logf("load=%d: p50 %v  p90 %v  p99 %v  max %v", load,
			pct(50).Round(10*time.Microsecond), pct(90).Round(10*time.Microsecond),
			pct(99).Round(10*time.Microsecond), samples[len(samples)-1].Round(10*time.Microsecond))
	}
}

// releaseReadResponses retires responses the way the client's release does,
// so the harness measures the same thing against code that predates release:
// without it a mailbox retains every response for MaxResponseAge and stops
// admitting at MaxResponseFiles within seconds. A response is long read once
// it is 100ms old.
func releaseReadResponses(ctx context.Context, dir string) {
	for ctx.Err() == nil {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			info, err := entry.Info()
			if err == nil && strings.HasSuffix(entry.Name(), ".res") && time.Since(info.ModTime()) > 100*time.Millisecond {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMeasureFastBehindSlow reports a fast query's latency while another
// client keeps a 200ms slow action running in the same mailbox, without lanes
// (every call dispatched in arrival order, as before) and with them. Opt-in:
// AMUX_MEASURE_RPC_LATENCY=1.
func TestMeasureFastBehindSlow(t *testing.T) {
	if os.Getenv("AMUX_MEASURE_RPC_LATENCY") == "" {
		t.Skip("set AMUX_MEASURE_RPC_LATENCY=1 to measure")
	}
	for _, lanes := range []bool{false, true} {
		options := ServerOptions{PollInterval: 50 * time.Millisecond}
		if lanes {
			options.Slow = func(call Call) bool { return call.Verb == "slow" }
		}
		f := newFixture(t, Callbacks{Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			if request.Call.Verb == "slow" {
				time.Sleep(200 * time.Millisecond)
			}
			return DispatchResult{Status: StatusOK, Body: []byte("ok")}, nil
		}}, options)
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan error, 1)
		go func() { served <- f.server.Run(ctx) }()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			releaseReadResponses(ctx, filepath.Join(f.grant.MailboxHostDir, ResponsesDirName))
		}()
		slowClient := f.client(clientOptions{})
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, _ = slowClient.Action(ctx, Action{Verb: "slow", ID: "subject-a"})
			}
		}()
		time.Sleep(50 * time.Millisecond)
		probe := f.client(clientOptions{})
		var samples []time.Duration
		for i := 0; i < 50; i++ {
			start := time.Now()
			callCtx, callCancel := context.WithTimeout(ctx, 10*time.Second)
			result, err := probe.Query(callCtx, Query{Verb: "version"})
			callCancel()
			if err != nil || result.Status != StatusOK {
				t.Fatalf("probe %d = %+v, %v", i, result, err)
			}
			samples = append(samples, time.Since(start))
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		wg.Wait()
		<-served
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		pct := func(p int) time.Duration { return samples[len(samples)*p/100] }
		t.Logf("lanes=%v: fast p50 %v  p90 %v  max %v", lanes,
			pct(50).Round(10*time.Microsecond), pct(90).Round(10*time.Microsecond), samples[len(samples)-1].Round(10*time.Microsecond))
	}
}
