package provider

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestConfigRelayIsTheConfiguredHarnessRelay(t *testing.T) {
	got := Config{Orchestrator: "tls://orch:7443", CAFile: "/ca.pem", ServerName: "orch.internal"}.Relay()
	want := HarnessRelay{Address: "tls://orch:7443", CAFile: "/ca.pem", ServerName: "orch.internal"}
	if got != want {
		t.Fatalf("Relay() = %+v, want %+v", got, want)
	}
}

// A broker that accepts TCP but never completes TLS must not pin the reconnect
// loop past operator stop: cancellation abandons the handshake.
func TestHarnessRelayDialHonorsCancellation(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, silent
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := HarnessRelay{Address: "tls:" + ln.Addr().String()}.Dial(ctx)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Dial error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay dial ignored cancellation")
	}
}
