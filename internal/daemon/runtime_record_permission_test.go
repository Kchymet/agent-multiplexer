package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/runtimeevents"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestPermissionBindingsDoNotRelabelHistoryAcrossRuntimeRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))

	dir := filepath.Join(home, "session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := core.RuntimeRecord{Runtime: "claude", Path: filepath.Join(dir, "transcript.jsonl"),
		Permissions: filepath.Join(dir, "authoritative-permissions.jsonl")}
	if err := os.MkdirAll(filepath.Dir(rec.Permissions), 0o700); err != nil {
		t.Fatal(err)
	}
	appendRequest := func(id string) {
		t.Helper()
		f, err := os.OpenFile(rec.Permissions, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(`{"request_id":"` + id + `","tool":"Bash","action":"echo ok"}` + "\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	appendRequest("historical")
	d := New("", nil, time.Hour)
	d.permissionBaseline = func(string) ([]string, error) {
		open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
		ids := make([]string, 0, len(open))
		for _, pending := range open {
			ids = append(ids, pending.RequestID)
		}
		return ids, nil
	}
	var currentRuntime *fakeInstance
	record := func() core.RuntimeRecord {
		out := rec
		out.PermissionBindings = make(map[string]string)
		open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
		for _, pending := range open {
			pending := pending
			generation, err := d.permissions.bindRequest("a1", pending.RequestID, currentRuntime, func() error {
				for _, candidate := range runtimeevents.OpenPermissions(runtimeEventRecord(rec)) {
					if candidate.Occurrence == pending.Occurrence {
						return nil
					}
				}
				return fmt.Errorf("permission occurrence closed")
			})
			if err == nil && generation != "" {
				out.PermissionBindings[pending.Occurrence] = generation
			}
		}
		return out
	}
	eng := newFakeEngine()
	d.engine = eng
	firstRuntime := eng.running("a1")
	currentRuntime = firstRuntime
	_, firstGeneration, err := d.publishPermissionRuntime("a1", func() (any, error) {
		return firstRuntime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := record()
	if len(first.PermissionBindings) != 0 {
		t.Fatalf("historical request rebound to first runtime: %v", first.PermissionBindings)
	}

	appendRequest("live-first")
	first = record()
	if len(first.PermissionBindings) != 1 || onlyBinding(first.PermissionBindings) != firstGeneration {
		t.Fatalf("first runtime bindings = %v", first.PermissionBindings)
	}

	d.permissions.retireAnd("a1", func() { eng.Kill(firstRuntime.Key()) })
	secondRuntime := eng.running("a1")
	currentRuntime = secondRuntime
	_, secondGeneration, err := d.publishPermissionRuntime("a1", func() (any, error) {
		return secondRuntime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := record()
	if len(restarted.PermissionBindings) != 0 {
		t.Fatalf("restart rebound unresolved history: %v", restarted.PermissionBindings)
	}

	appendRequest("live-second")
	restarted = record()
	if len(restarted.PermissionBindings) != 1 || onlyBinding(restarted.PermissionBindings) != secondGeneration {
		t.Fatalf("second runtime bindings = %v", restarted.PermissionBindings)
	}

	// Drive the real normalized producer through fresh daemon resolution at both
	// admission and publication. Replay sees all three unresolved journal entries,
	// but publishes only the request bound to the second runtime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := runtimeevents.Stream(func(id string) (runtimeevents.Record, bool) {
		return runtimeEventRecord(record()), id == "a1"
	}, time.Millisecond)
	ch, ok := stream(ctx, "a1", 0)
	if !ok {
		t.Fatal("runtime event stream rejected daemon record")
	}
	select {
	case batch := <-ch:
		if len(batch.Events) != 3 {
			t.Fatalf("published events = %+v", batch.Events)
		}
		for i, event := range batch.Events {
			if event.Type != harnessproto.TypePermissionRequest {
				t.Fatalf("event %d type = %q", i, event.Type)
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			generation, answerable := payload[harnessproto.FieldRuntimeGeneration]
			if i < 2 && answerable {
				t.Fatalf("historical event %d was relabeled answerable: %v", i, payload)
			}
			if i == 2 && (payload[harnessproto.FieldRequestID] != "live-second" || generation != secondGeneration) {
				t.Fatalf("published live permission tuple = %v", payload)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bound permission replay")
	}

	// The live resolution must retain the generation of the request occurrence
	// that was actually published, even though the daemon no longer reports the
	// request open when the resolution reaches the tailer.
	appendRequest = func(id string) {
		t.Helper()
		f, err := os.OpenFile(rec.Permissions, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(`{"request_id":"` + id + `","decision":"allow"}` + "\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	appendRequest("live-second")
	select {
	case batch := <-ch:
		if len(batch.Events) != 1 || batch.Events[0].Type != harnessproto.TypePermissionResolved {
			t.Fatalf("live resolution events = %+v", batch.Events)
		}
		var payload map[string]any
		if err := json.Unmarshal(batch.Events[0].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload[harnessproto.FieldRuntimeGeneration] != secondGeneration {
			t.Fatalf("live resolution generation = %v, want %q", payload, secondGeneration)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bound permission resolution")
	}
	cancel()
	for range ch {
	}

	// Reuse exactly the same native request id in a third runtime. A fresh replay
	// must leave the old request/resolution readable but generation-free and bind
	// only the new occurrence. The two occurrence ItemIDs must be distinct.
	d.permissions.retire("a1")
	eng.mu.Lock()
	delete(eng.insts, secondRuntime.Key())
	eng.mu.Unlock()
	thirdRuntime := eng.running("a1")
	currentRuntime = thirdRuntime
	_, thirdGeneration, err := d.publishPermissionRuntime("a1", func() (any, error) {
		return thirdRuntime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	appendRequest = func(id string) {
		t.Helper()
		f, err := os.OpenFile(rec.Permissions, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(`{"request_id":"` + id + `","tool":"Bash","action":"echo reused"}` + "\n")
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	appendRequest("live-second")
	reused := record()
	if len(reused.PermissionBindings) != 1 || onlyBinding(reused.PermissionBindings) != thirdGeneration {
		t.Fatalf("reused-id runtime bindings = %v", reused.PermissionBindings)
	}
	replayCtx, replayCancel := context.WithCancel(context.Background())
	replay := runtimeevents.Stream(func(id string) (runtimeevents.Record, bool) {
		return runtimeEventRecord(record()), id == "a1"
	}, time.Millisecond)
	replayCh, ok := replay(replayCtx, "a1", 0)
	if !ok {
		t.Fatal("reused-id replay rejected daemon record")
	}
	select {
	case batch := <-replayCh:
		var reusedEvents []harnessproto.RuntimeEvent
		for _, event := range batch.Events {
			var payload map[string]any
			if json.Unmarshal(event.Payload, &payload) == nil && payload[harnessproto.FieldRequestID] == "live-second" {
				reusedEvents = append(reusedEvents, event)
			}
		}
		if len(reusedEvents) != 3 {
			t.Fatalf("same-id replay events = %+v, want old request/resolution and new request", reusedEvents)
		}
		if reusedEvents[0].ItemID == "" || reusedEvents[0].ItemID != reusedEvents[1].ItemID || reusedEvents[2].ItemID == reusedEvents[0].ItemID {
			t.Fatalf("same-id occurrence keys = %q, %q, %q", reusedEvents[0].ItemID, reusedEvents[1].ItemID, reusedEvents[2].ItemID)
		}
		for i, event := range reusedEvents {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			generation, present := payload[harnessproto.FieldRuntimeGeneration]
			if i < 2 && present {
				t.Fatalf("old same-id event %d relabeled with current generation: %v", i, payload)
			}
			if i == 2 && generation != thirdGeneration {
				t.Fatalf("new same-id request generation = %v, want %q", payload, thirdGeneration)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for same-id replay")
	}
	replayCancel()
	for range replayCh {
	}
}

func onlyBinding(bindings map[string]string) string {
	for _, generation := range bindings {
		return generation
	}
	return ""
}
