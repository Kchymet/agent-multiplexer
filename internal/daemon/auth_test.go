package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
)

func TestAuthReloadDefersBusyAndUnknownAndOnlyResumesClaude(t *testing.T) {
	isolateHome(t)
	if err := claudecfg.Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	e := newFakeEngine()
	d.engine = e
	d.resolve = func(id string, tab int) (string, []string, []string, error) {
		return "/session/" + id, []string{"AUTH=shared"}, []string{"claude", "--resume", convID(id)}, nil
	}
	for _, id := range []string{"idle", "busy", "unknown", "codex", "closed", "replaced"} {
		kind := "claude"
		if id == "codex" {
			kind = "codex"
		}
		putSession(t, id, kind)
		e.running(id)
	}
	for _, id := range []string{"idle", "closed", "replaced"} {
		if err := core.WriteHookState(convID(id), core.StateReady, ""); err != nil {
			t.Fatal(err)
		}
	}
	markBusy(t, convID("busy"))
	shell := engine.Key{AgentID: "idle", Tab: panespec.TabTerminal}
	e.insts[shell] = &fakeInstance{key: shell}
	if r := d.handle(context.Background(), core.Action{Action: core.ActionAuthReload}); !r.OK {
		t.Fatal(r.Error)
	}
	e.Kill(engine.Key{AgentID: "closed"})
	e.Kill(engine.Key{AgentID: "replaced"})
	e.running("replaced")
	d.resumeWithSharedAuth(context.Background())
	if got := e.ensuredKeys(); len(got) != 1 || got[0].AgentID != "idle" {
		t.Fatalf("must resume only idle Claude: %v", got)
	}
	if len(d.authPending) != 2 {
		t.Fatalf("pending = %v", d.authPending)
	}
	if _, ok := e.Lookup(shell); !ok {
		t.Fatal("shell stopped")
	}
	if err := core.WriteHookState(convID("busy"), core.StateReady, ""); err != nil {
		t.Fatal(err)
	}
	d.resumeWithSharedAuth(context.Background())
	if got := e.ensuredKeys(); len(got) != 2 || got[1].AgentID != "busy" {
		t.Fatalf("busy Claude should resume after finishing its turn: %v", got)
	}
	if err := d.queueAuthReload(core.Action{Fields: map[string]string{"force": "true"}}); err != nil {
		t.Fatal(err)
	}
	d.resumeWithSharedAuth(context.Background())
	if len(d.authPending) != 0 {
		t.Fatal("force left a stuck login prompt pending")
	}
	for _, k := range e.ensuredKeys() {
		if k.AgentID == "codex" || k.Tab != panespec.TabAgent || k.AgentID == "closed" {
			t.Fatalf("restarted unrelated/closed pane: %v", k)
		}
	}
}

func TestAuthReloadResolveFailurePreservesProcess(t *testing.T) {
	isolateHome(t)
	if err := claudecfg.Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	putSession(t, "idle", "claude")
	d := New("", nil, time.Hour)
	e := newFakeEngine()
	d.engine = e
	original := e.running("idle")
	d.resolve = func(string, int) (string, []string, []string, error) {
		return "", nil, nil, fmt.Errorf("missing executable")
	}
	if err := d.queueAuthReload(core.Action{Fields: map[string]string{"force": "true"}}); err != nil {
		t.Fatal(err)
	}
	d.resumeWithSharedAuth(context.Background())
	got, ok := e.Lookup(original.Key())
	if !ok || got != original {
		t.Fatal("failed resolution killed existing process")
	}
}

func TestAuthReloadDoesNotCarryForceToReplacementProcess(t *testing.T) {
	isolateHome(t)
	if err := claudecfg.Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	putSession(t, "busy", "claude")
	markBusy(t, convID("busy"))
	d := New("", nil, time.Hour)
	e := newFakeEngine()
	d.engine = e
	e.running("busy")
	if err := d.queueAuthReload(core.Action{Fields: map[string]string{"force": "true"}}); err != nil {
		t.Fatal(err)
	}
	k := engine.Key{AgentID: "busy", Tab: panespec.TabAgent}
	e.Kill(k)
	replacement := e.running("busy")
	if err := d.queueAuthReload(core.Action{}); err != nil {
		t.Fatal(err)
	}
	d.resumeWithSharedAuth(context.Background())
	if in, ok := e.Lookup(k); !ok || in != replacement || d.authPending[k].force {
		t.Fatal("an earlier force request interrupted a replacement process")
	}
}
