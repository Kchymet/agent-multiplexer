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

func bindPermission(t *testing.T, gate *runtimePermissionGate, subject, requestID string, runtime any) string {
	t.Helper()
	generation, err := gate.bindRequest(subject, requestID, runtime, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestPermissionBindingRequiresExactRuntimeAndExcludesBaseline(t *testing.T) {
	gate := newRuntimePermissionGate()
	first := new(int)
	generation, err := gate.observeExcluding("a1", first, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.bindRequest("a1", "old", first, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("historical request binding = %v", err)
	}
	if got, err := gate.bindRequest("a1", "new", first, func() error { return nil }); err != nil || got != generation {
		t.Fatalf("live request binding = %q, %v", got, err)
	}
	replacement := new(int)
	if _, err := gate.observeExcluding("a1", replacement, []string{"old", "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.bindRequest("a1", "new", first, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("old handle binding after replacement = %v", err)
	}
}

func TestRuntimePermissionGateConsumesExactlyOnceConcurrently(t *testing.T) {
	gate := newRuntimePermissionGate()
	runtime := &permissionRuntimeFixture{}
	generation, err := gate.observe("a1", runtime)
	if err != nil {
		t.Fatal(err)
	}
	bindPermission(t, gate, "a1", "request-1", runtime)
	var delivered atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- gate.consume("a1", generation, "request-1", runtime, func() error { return nil }, func() error {
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
	bindPermission(t, gate, "a1", "r1", first)
	wantErr := errors.New("acknowledgement lost")
	if err := gate.consume("a1", firstGeneration, "r1", first, func() error { return nil }, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("first consume = %v", err)
	}
	if err := gate.consume("a1", firstGeneration, "r1", first, func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("retry after uncertain delivery = %v", err)
	}
	second := &permissionRuntimeFixture{n: 2}
	secondGeneration, err := gate.observe("a1", second)
	if err != nil {
		t.Fatal(err)
	}
	if secondGeneration == firstGeneration {
		t.Fatal("replacement runtime reused its predecessor generation")
	}
	bindPermission(t, gate, "a1", "r1", second)
	if err := gate.consume("a1", firstGeneration, "r2", first, func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("old generation consume = %v", err)
	}
	if err := gate.consume("a1", secondGeneration, "r1", second, func() error { return nil }, func() error { return nil }); err != nil {
		t.Fatalf("request id must be reusable only by a new runtime generation: %v", err)
	}
}

func TestRuntimePermissionGateClaimsBeforeFinalValidation(t *testing.T) {
	gate := newRuntimePermissionGate()
	runtime := &permissionRuntimeFixture{}
	generation, err := gate.observe("a1", runtime)
	if err != nil {
		t.Fatal(err)
	}
	bindPermission(t, gate, "a1", "r1", runtime)
	changed := errors.New("prompt changed")
	if err := gate.consume("a1", generation, "r1", runtime, func() error { return changed }, func() error {
		t.Fatal("delivery ran after final validation failed")
		return nil
	}); !errors.Is(err, changed) {
		t.Fatalf("validation result = %v", err)
	}
	if err := gate.consume("a1", generation, "r1", runtime, func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "already consumed") {
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
	bindPermission(t, gate, "a1", "r1", first)
	delivering := make(chan struct{})
	release := make(chan struct{})
	consumed := make(chan error, 1)
	go func() {
		consumed <- gate.consume("a1", generation, "r1", first, func() error { return nil }, func() error {
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

func TestRuntimePermissionGatePublishesReplacementBeforeOldGenerationCanDeliver(t *testing.T) {
	gate := newRuntimePermissionGate()
	oldRuntime := &permissionRuntimeFixture{n: 1}
	oldGeneration, err := gate.observe("a1", oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	bindPermission(t, gate, "a1", "request-old", oldRuntime)
	newRuntime := &permissionRuntimeFixture{n: 2}
	published := make(chan struct{})
	releaseCreate := make(chan struct{})
	publication := make(chan error, 1)
	go func() {
		_, _, err := gate.publish("a1", func() (any, error) {
			// This is the Engine.Ensure boundary: the new handle is externally
			// visible before Ensure returns to its caller.
			close(published)
			<-releaseCreate
			return newRuntime, nil
		})
		publication <- err
	}()
	<-published

	delivered := make(chan error, 1)
	go func() {
		delivered <- gate.consume("a1", oldGeneration, "request-old", oldRuntime,
			func() error { return nil }, func() error { return nil })
	}()
	select {
	case err := <-delivered:
		t.Fatalf("old generation crossed in-progress runtime publication: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseCreate)
	if err := <-publication; err != nil {
		t.Fatal(err)
	}
	if err := <-delivered; err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("old generation after replacement publication = %v", err)
	}
}

func TestPermissionHistoryReplayCannotAcquireReplacementGeneration(t *testing.T) {
	gate := newRuntimePermissionGate()
	oldRuntime := &permissionRuntimeFixture{n: 1}
	oldGeneration, err := gate.observe("a1", oldRuntime)
	if err != nil {
		t.Fatal(err)
	}
	bindPermission(t, gate, "a1", "historical-request", oldRuntime)

	newRuntime := &permissionRuntimeFixture{n: 2}
	_, newGeneration, err := gate.publish("a1", func() (any, error) { return newRuntime, nil })
	if err != nil {
		t.Fatal(err)
	}
	notOpen := errors.New("historical request is not open on replacement")
	if _, err := gate.bindRequest("a1", "historical-request", newRuntime, func() error { return notOpen }); !errors.Is(err, notOpen) {
		t.Fatalf("historical replay binding = %v", err)
	}
	if err := gate.consume("a1", oldGeneration, "historical-request", oldRuntime,
		func() error { return nil }, func() error { return nil }); err == nil {
		t.Fatal("old runtime generation survived replacement")
	}
	if err := gate.consume("a1", newGeneration, "historical-request", newRuntime,
		func() error { return nil }, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "not observed") {
		t.Fatalf("historical request inherited replacement generation: %v", err)
	}

	bindPermission(t, gate, "a1", "current-request", newRuntime)
	if err := gate.consume("a1", newGeneration, "current-request", newRuntime,
		func() error { return nil }, func() error { return nil }); err != nil {
		t.Fatalf("current replacement request was not deliverable: %v", err)
	}
}
