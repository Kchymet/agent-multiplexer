package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
)

func TestRestartAndDaemonRestoreContinueOnlyRunningWork(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		for _, state := range []string{core.StateRunning, core.StateReady, core.StateWaiting, core.StateIdle, core.StateUnknown} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				d, e := steerDaemon(t)
				putSession(t, "a1", kind)
				e.running("a1")
				if err := core.WriteSessionHookState("a1", convID("a1"), state, ""); err != nil {
					t.Fatal(err)
				}
				want := state == core.StateRunning
				launches := 0
				d.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
					launches++
					if spec.ResumeWork != want || spec.Session.ClaudeID != convID("a1") {
						t.Fatal("restart lost work intent or conversation")
					}
					return "", nil, []string{kind}, nil
				}
				if r := d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "a1"}); !r.OK {
					t.Fatal(r.Error)
				}
				d.liveAgentsPath = filepath.Join(t.TempDir(), "live.json")
				d.persistLiveAgents()
				d.pendingRestore = d.readLiveAgents()
				if len(d.pendingRestore) != 1 || d.pendingRestore[0].ResumeWork != want {
					t.Fatal("journal lost pre-shutdown intent")
				}
				// Exit hooks can mark the old runtime idle after intent is saved.
				core.WriteSessionHookState("a1", convID("a1"), core.StateIdle, "")
				e.Kill(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
				d.sessions = []core.Session{{ID: "a1"}}
				d.restoreLiveAgents(context.Background())
				if launches != 2 {
					t.Fatalf("launches=%d", launches)
				}
			})
		}
	}
}

func TestRestartIntentDoesNotCrossConversationOrHarness(t *testing.T) {
	d, _ := steerDaemon(t)
	putSession(t, "a1", "claude")
	markBusy(t, "a1")
	r := d.captureRestart(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
	spec, err := d.launchSpec(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	spec.Session.ClaudeID = "another"
	if r.apply(&spec) != nil || spec.ResumeWork {
		t.Fatal("resumed another conversation")
	}
	spec.Session.ClaudeID = convID("a1")
	spec.Session.Agent = "codex"
	if r.apply(&spec) != nil || spec.ResumeWork {
		t.Fatal("resumed another harness")
	}
}
