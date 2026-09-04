package provider

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestWriteReadStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "provider-status.json")
	if _, err := ReadStatus(path); !os.IsNotExist(err) {
		t.Fatalf("ReadStatus with no file = %v, want a not-exist error", err)
	}

	want := Status{
		PID: 4242, Name: "box", Orchestrator: "orch:7443", State: StateRegistered,
		ProviderID: "prov-1", Panes: 2,
		RegisteredAt: time.Now().Add(-time.Minute).Round(time.Millisecond),
		HeartbeatAt:  time.Now().Round(time.Millisecond),
		LastError:    "",
	}
	if err := WriteStatus(path, want); err != nil {
		t.Fatalf("WriteStatus: %v", err)
	}
	got, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if got.PID != want.PID || got.State != want.State || got.ProviderID != want.ProviderID || got.Panes != want.Panes {
		t.Errorf("ReadStatus = %+v, want %+v", got, want)
	}
	if !got.RegisteredAt.Equal(want.RegisteredAt) || !got.HeartbeatAt.Equal(want.HeartbeatAt) {
		t.Errorf("moments did not survive the round trip: %+v", got)
	}

	// The write is atomic (temp + rename), so no reader ever sees a half-written
	// record and no scratch file is left lying beside the real one.
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != filepath.Base(path) {
		t.Errorf("directory holds %d entries, want just the status file", len(ents))
	}
}

// TestStatusReportsTheConnectionLifecycle drives the real loop against the fake
// orchestrator and pins the states a report depends on: dialing → registered,
// with the providerId recorded, then disconnected when the connection drops and
// stopped on an operator stop. Without this, doctor could report "registered"
// for a provider that was never accepted.
func TestStatusReportsTheConnectionLifecycle(t *testing.T) {
	var mu sync.Mutex
	var seen []Status
	record := func(s Status) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, s)
	}
	states := func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		for i, s := range seen {
			out[i] = s.State
		}
		return out
	}
	last := func() Status {
		mu.Lock()
		defer mu.Unlock()
		if len(seen) == 0 {
			return Status{}
		}
		return seen[len(seen)-1]
	}

	conns := make(chan net.Conn, 1)
	p := newFast(Config{
		Orchestrator: "pipe", Name: "box", Dial: pipeDialer(conns), OnStatus: record,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	waitFor(t, func() bool { return last().State == StateRegistered })

	reg := last()
	if reg.ProviderID != "prov-test" || reg.Name != "box" || reg.Orchestrator != "pipe" {
		t.Errorf("registered status = %+v, want the orchestrator's identity recorded", reg)
	}
	if reg.RegisteredAt.IsZero() {
		t.Errorf("registered status has no RegisteredAt: %+v", reg)
	}
	if got := states(); got[0] != StateDialing {
		t.Errorf("first state = %q, want %q", got[0], StateDialing)
	}

	// A dropped connection must not keep reporting "registered": that is exactly
	// the case where a log tail looks healthy and the provider is not.
	oc.Close()
	waitFor(t, func() bool { return last().State == StateDisconnected })
	if got := last(); got.RegisteredAt.IsZero() {
		t.Errorf("disconnect cleared RegisteredAt; a report can no longer say when it last worked: %+v", got)
	}

	cancel()
	<-runErr
	waitFor(t, func() bool { return last().State == StateStopped })
}

// TestStatusRecordsATerminalRejection: a revoked token stops the loop for good,
// and the status file is the only place that says why.
func TestStatusRecordsATerminalRejection(t *testing.T) {
	var mu sync.Mutex
	var last Status
	conns := make(chan net.Conn, 1)
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		OnStatus: func(s Status) { mu.Lock(); last = s; mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-conns)
	expectRegister(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: false, Error: harnessproto.ErrRevoked,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-runErr; err == nil {
		t.Fatal("Run = nil, want the terminal rejection")
	}

	mu.Lock()
	defer mu.Unlock()
	if last.State != StateRejected {
		t.Errorf("state = %q, want %q", last.State, StateRejected)
	}
	if last.LastError == "" {
		t.Errorf("rejection recorded no reason: %+v", last)
	}
}

// waitFor polls cond until it holds, failing the test if it never does.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition never held within the timeout")
}
