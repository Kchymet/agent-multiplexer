package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/runtimeevents"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestPermissionBindingsDoNotRelabelHistoryAcrossRuntimeRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "session")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "a1", RootID: "wg", Agent: "claude", Dir: dir,
		ClaudeID: "33333333-3333-4333-8333-333333333333"}); err != nil {
		t.Fatal(err)
	}
	rec, err := (&Daemon{permissions: newRuntimePermissionGate()}).runtimeRecordRaw(db, "a1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
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
	engine := newFakeEngine()
	d.engine = engine
	firstRuntime := engine.running("a1")
	_, firstGeneration, err := d.publishPermissionRuntime("a1", func() (any, error) {
		return firstRuntime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db, _ = store.Open()
	first, err := d.runtimeRecord(db, "a1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.PermissionBindings) != 0 {
		t.Fatalf("historical request rebound to first runtime: %v", first.PermissionBindings)
	}

	appendRequest("live-first")
	db, _ = store.Open()
	first, err = d.runtimeRecord(db, "a1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if first.PermissionBindings["live-first"] != firstGeneration || first.PermissionBindings["historical"] != "" {
		t.Fatalf("first runtime bindings = %v", first.PermissionBindings)
	}

	d.permissions.retire("a1")
	engine.mu.Lock()
	delete(engine.insts, firstRuntime.Key())
	engine.mu.Unlock()
	secondRuntime := engine.running("a1")
	_, secondGeneration, err := d.publishPermissionRuntime("a1", func() (any, error) {
		return secondRuntime, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db, _ = store.Open()
	restarted, err := d.runtimeRecord(db, "a1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.PermissionBindings) != 0 {
		t.Fatalf("restart rebound unresolved history: %v", restarted.PermissionBindings)
	}

	appendRequest("live-second")
	db, _ = store.Open()
	restarted, err = d.runtimeRecord(db, "a1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if restarted.PermissionBindings["live-second"] != secondGeneration ||
		restarted.PermissionBindings["live-first"] != "" || restarted.PermissionBindings["historical"] != "" {
		t.Fatalf("second runtime bindings = %v", restarted.PermissionBindings)
	}

	// Drive the real normalized producer through fresh daemon resolution at both
	// admission and publication. Replay sees all three unresolved journal entries,
	// but publishes only the request bound to the second runtime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := runtimeevents.Stream(func(id string) (runtimeevents.Record, bool) {
		db, err := store.Open()
		if err != nil {
			t.Errorf("open store from stream resolver: %v", err)
			return runtimeevents.Record{}, false
		}
		defer db.Close()
		current, err := d.runtimeRecord(db, id)
		if err != nil {
			t.Errorf("resolve runtime record from stream: %v", err)
			return runtimeevents.Record{}, false
		}
		return runtimeEventRecord(current), true
	}, time.Millisecond)
	ch, ok := stream(ctx, "a1", 0)
	if !ok {
		t.Fatal("runtime event stream rejected daemon record")
	}
	select {
	case batch := <-ch:
		if len(batch.Events) != 1 || batch.Events[0].Type != harnessproto.TypePermissionRequest {
			t.Fatalf("published events = %+v", batch.Events)
		}
		var payload map[string]any
		if err := json.Unmarshal(batch.Events[0].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload[harnessproto.FieldRequestID] != "live-second" ||
			payload[harnessproto.FieldRuntimeGeneration] != secondGeneration {
			t.Fatalf("published permission tuple = %v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for bound permission replay")
	}
}
