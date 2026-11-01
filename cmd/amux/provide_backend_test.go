package main

import (
	"context"
	"testing"

	"amux/internal/core"
	"amux/internal/hostbackend/hostbackendtest"
	"amux/internal/provider"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// `amux provide` routes every session-facing hook through the shared host
// backend, and the publication grants still decide which hooks exist.
func TestWireHostBackendRoutesPublishedHooks(t *testing.T) {
	snap, verb, rec, acct := hostbackendtest.New("snap"), hostbackendtest.New("verb"), hostbackendtest.New("rec"), hostbackendtest.New("acct")
	snap.SnapshotFn = func() ([]core.Session, error) { return []core.Session{{ID: "s1"}}, nil }
	verb.DispatchFn = func(core.Action) (string, error) { return "n1", nil }
	rec.RuntimeRecordFn = func(id string) (core.RuntimeRecord, error) {
		return core.RuntimeRecord{Runtime: "claude", Path: "/x/" + id + ".jsonl"}, nil
	}
	d := hostbackendtest.NewDialer(snap, verb, rec, acct)
	var cfg provider.Config
	wireHostBackend(&cfg, d.Backend(), true, false, true)
	if !cfg.PublishSessions || cfg.ReadOnlySessions || !cfg.RuntimeEvents ||
		cfg.Sessions == nil || cfg.ApplyAction == nil || cfg.RuntimeEventStream == nil || cfg.RuntimeEventHistory == nil || cfg.OpenAccounts == nil {
		t.Fatalf("writable publication wiring incomplete: %+v", cfg)
	}
	ctx := context.Background()
	if s, err := cfg.Sessions(ctx); err != nil || len(s) != 1 || s[0].ID != "s1" {
		t.Fatalf("Sessions = %v, %v", s, err)
	}
	if id, err := cfg.ApplyAction(ctx, core.Action{Action: core.ActionStart}); err != nil || id != "n1" {
		t.Fatalf("ApplyAction = %q, %v", id, err)
	}
	if r, ok := runtimeRecordResolver(d.Backend())(ctx, "s1"); !ok || r.Path != "/x/s1.jsonl" || r.Runtime != "claude" {
		t.Fatalf("resolver = %+v, %v", r, ok)
	}
	ch, err := cfg.OpenAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Call(ctx, hp.AccountRequest{Operation: hp.AccountStatus}, false); err != nil {
		t.Fatal(err)
	}
	_ = ch.Close()
	if got := acct.Calls(); len(got) == 0 || got[0] != "accounts:"+hp.AccountStatus || !acct.IsClosed() {
		t.Fatalf("account calls = %v closed=%v", got, acct.IsClosed())
	}
}

func TestWireHostBackendKeepsGrantsFailClosed(t *testing.T) {
	d := hostbackendtest.NewDialer()

	var none provider.Config
	wireHostBackend(&none, d.Backend(), false, false, false)
	if none.PublishSessions || none.Sessions != nil || none.ApplyAction != nil || none.RuntimeEventStream != nil || none.OpenAccounts == nil {
		t.Fatalf("unpublished wiring = %+v", none)
	}

	var ro provider.Config
	wireHostBackend(&ro, d.Backend(), true, true, false)
	if !ro.PublishSessions || !ro.ReadOnlySessions || ro.Sessions == nil || ro.ApplyAction != nil || ro.RuntimeEvents || ro.RuntimeEventStream != nil {
		t.Fatalf("read-only wiring = %+v", ro)
	}

	// A resolver miss (no reachable daemon) degrades to no events, not an error.
	if _, ok := runtimeRecordResolver(d.Backend())(context.Background(), "s1"); ok {
		t.Fatal("resolver reported a record with no daemon")
	}
	if d.Dials() != 1 {
		t.Fatalf("dials = %d", d.Dials())
	}
}
