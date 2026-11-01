package provider

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// tcpDialer dials a real loopback listener so the provider's pings land in
// kernel socket buffers even while the fake orchestrator never reads them —
// the shape of a broker that has stopped reading to apply flow control.
func tcpDialer(t *testing.T) (func(context.Context) (net.Conn, error), <-chan net.Conn, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			accepted <- c
		}
	}()
	var dials atomic.Int32
	dial := func(ctx context.Context) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ln.Addr().String())
	}
	return dial, accepted, &dials
}

// TestHeartbeatSurvivesOrchestratorFlowControl: the orchestrator stops reading
// (its pings go unanswered) but keeps the connection alive with unsolicited
// pongs or other frames. The provider must not declare the connection dead —
// that drop is what greyed every session of the machine at once. Without them,
// the 4-interval liveness timeout still fires.
func TestHeartbeatSurvivesOrchestratorFlowControl(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keepalive *harnessproto.MuxMsg
		wantAlive bool
	}{
		{name: "unsolicited pongs", keepalive: &harnessproto.MuxMsg{Type: harnessproto.MPong}, wantAlive: true},
		{name: "any other frame", keepalive: &harnessproto.MuxMsg{Type: "x-future-frame"}, wantAlive: true},
		{name: "silence still times out", wantAlive: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dial, accepted, dials := tcpDialer(t)
			p := newFast(Config{Orchestrator: "tcp", Dial: dial})
			p.hbScale = 20 * time.Millisecond // heartbeat 1 ⇒ 20ms; dead after 80ms
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runErr := make(chan error, 1)
			go func() { runErr <- p.Run(ctx) }()

			oc := harnessproto.NewConn(<-accepted)
			accept(t, oc, 2, nil, 60)
			// From here the orchestrator never reads again: pings pile up unread.
			stop := time.After(500 * time.Millisecond) // > 6 liveness windows
			tick := time.NewTicker(30 * time.Millisecond)
			defer tick.Stop()
		loop:
			for {
				select {
				case <-stop:
					break loop
				case <-tick.C:
					if tc.keepalive != nil {
						if err := oc.WriteMux(*tc.keepalive); err != nil {
							t.Fatalf("keepalive write failed — provider dropped the connection: %v", err)
						}
					}
				}
			}
			alive := dials.Load() == 1
			if alive != tc.wantAlive {
				t.Fatalf("dials = %d, want connection alive=%v", dials.Load(), tc.wantAlive)
			}
			cancel()
			<-runErr
		})
	}
}
