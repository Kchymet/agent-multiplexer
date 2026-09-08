package daemon

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type permissionRuntimeFixture struct{ n int }

func TestRuntimePermissionGateConsumesExactlyOnceConcurrently(t *testing.T) {
	gate := newRuntimePermissionGate()
	runtime := &permissionRuntimeFixture{}
	generation, err := gate.observe("a1", runtime)
	if err != nil {
		t.Fatal(err)
	}
	var delivered atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- gate.consume("a1", generation, "request-1", func() error { return nil }, func() error {
				delivered.Add(1)
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)
	if got := delivered.Load(); got != 1 {
		t.Fatalf("delivery count = %d, want 1", got)
	}
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted decisions = %d, want 1", accepted)
	}
}

func TestRuntimePermissionGateBindsGenerationAndConsumesFailures(t *testing.T) {
	gate := newRuntimePermissionGate()
	first := &permissionRuntimeFixture{n: 1}
	firstGeneration, err := gate.observe("a1", first)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("acknowledgement lost")
	if err := gate.consume("a1", firstGeneration, "r1", func() error { return nil }, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("first consume = %v", err)
	}
	if err := gate.consume("a1", firstGeneration, "r1", func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("retry after uncertain delivery = %v", err)
	}
	secondGeneration, err := gate.observe("a1", &permissionRuntimeFixture{n: 2})
	if err != nil {
		t.Fatal(err)
	}
	if secondGeneration == firstGeneration {
		t.Fatal("replacement runtime reused its predecessor generation")
	}
	if err := gate.consume("a1", firstGeneration, "r2", func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("old generation consume = %v", err)
	}
	if err := gate.consume("a1", secondGeneration, "r1", func() error { return nil }, func() error { return nil }); err != nil {
		t.Fatalf("request id must be reusable only by a new runtime generation: %v", err)
	}
}

func TestRuntimePermissionGateClaimsBeforeFinalValidation(t *testing.T) {
	gate := newRuntimePermissionGate()
	generation, err := gate.observe("a1", &permissionRuntimeFixture{})
	if err != nil {
		t.Fatal(err)
	}
	changed := errors.New("prompt changed")
	if err := gate.consume("a1", generation, "r1", func() error { return changed }, func() error {
		t.Fatal("delivery ran after final validation failed")
		return nil
	}); !errors.Is(err, changed) {
		t.Fatalf("validation result = %v", err)
	}
	if err := gate.consume("a1", generation, "r1", func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("failed final validation did not consume claim: %v", err)
	}
}

func TestRuntimePermissionGateSerializesReplacementWithDelivery(t *testing.T) {
	gate := newRuntimePermissionGate()
	first := &permissionRuntimeFixture{n: 1}
	generation, err := gate.observe("a1", first)
	if err != nil {
		t.Fatal(err)
	}
	delivering := make(chan struct{})
	release := make(chan struct{})
	consumed := make(chan error, 1)
	go func() {
		consumed <- gate.consume("a1", generation, "r1", func() error { return nil }, func() error {
			close(delivering)
			<-release
			return nil
		})
	}()
	<-delivering
	replaced := make(chan string, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		next, _ := gate.observe("a1", &permissionRuntimeFixture{n: 2})
		replaced <- next
	}()
	<-started
	select {
	case <-replaced:
		t.Fatal("runtime replacement crossed an in-flight permission delivery")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-consumed; err != nil {
		t.Fatal(err)
	}
	if next := <-replaced; next == generation {
		t.Fatal("replacement reused the delivered runtime generation")
	}
}
