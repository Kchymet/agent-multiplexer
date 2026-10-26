package daemon

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/engine"
)

// launchEngine respawns an exited instance on Ensure, like the local engine.
// exitAfterLookup makes the next Lookup report the pane alive and then exit it,
// reproducing a pane that dies between classification and Ensure.
type launchEngine struct {
	*fakeEngine
	exitAfterLookup bool
}

func (e *launchEngine) Lookup(k engine.Key) (engine.Instance, bool) {
	in, ok := e.fakeEngine.Lookup(k)
	if ok && e.exitAfterLookup {
		e.exitAfterLookup = false
		f := in.(*fakeInstance)
		f.mu.Lock()
		f.dead = true
		f.mu.Unlock()
	}
	return in, ok
}

func (e *launchEngine) Ensure(ctx context.Context, spec engine.Spec) (engine.Instance, error) {
	e.mu.Lock()
	if in, ok := e.insts[spec.Key]; ok && !in.Alive() {
		delete(e.insts, spec.Key)
	}
	e.mu.Unlock()
	return e.fakeEngine.Ensure(ctx, spec)
}

// Every process the engine actually starts holds a valid fresh launch; a
// resolve whose argv started nothing (reused pane, failed spawn) is retired.
func TestEnsurePaneRetiresOnlyLaunchesThatStartedNothing(t *testing.T) {
	isolatedAccounts(t)
	claudeReady := func() uint64 {
		tok, _ := centralauth.NewSecret("SYNTHETIC-PANE-TOKEN")
		c, err := centralauth.ClaudeStore().Save(tok, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := accounts.Update(func(s *accounts.State) error { s.CredentialVersion["claude:central"] = c.Generation; return nil }); err != nil {
			t.Fatal(err)
		}
		return c.Generation
	}
	gen := claudeReady()
	key := engine.Key{AgentID: "pane", Tab: 0}
	eng := &launchEngine{fakeEngine: newFakeEngine()}
	d := New("", nil, time.Hour)
	d.engine = eng
	spec := func() (engine.Spec, accounts.Launch) {
		l, err := accounts.BeginLaunch("pane", "claude")
		if err != nil {
			t.Fatal(err)
		}
		return engine.Spec{Key: key, Argv: []string{"bwrap", "--", "amux", "--amux-payload-clean-exec", centralauth.LauncherArg,
			centralauth.LaunchArgPrefix + strconv.FormatUint(l.Seq, 10) + ":" + l.Nonce, "claude"}}, l
	}
	pending := func(l accounts.Launch) bool {
		s, _ := accounts.Read()
		for _, q := range s.Bindings["pane"].Pending {
			if q.Seq == l.Seq {
				return true
			}
		}
		return false
	}
	accept := func(l accounts.Launch) error {
		b, p, err := accounts.Delivery("pane", "claude")
		if err != nil {
			return err
		}
		return accounts.Accept("pane", b.Revision, p.DeliveryEpoch, "", gen, l)
	}

	first, firstLaunch := spec()
	running, err := d.ensurePane(context.Background(), first)
	if err != nil || !pending(firstLaunch) {
		t.Fatalf("fresh spawn lost its launch: %v", err)
	}
	if err := accept(firstLaunch); err != nil {
		t.Fatal("spawned pane refused its launch", err)
	}
	// Reattach: the running pane is reused, so the new allocation is retired.
	reattach, reattachLaunch := spec()
	if in, err := d.ensurePane(context.Background(), reattach); err != nil || in != running || pending(reattachLaunch) {
		t.Fatalf("reattach kept an unused launch: %v", err)
	}
	if err := accept(reattachLaunch); err == nil {
		t.Fatal("retired launch admitted")
	}
	// The pane exits between classification and Ensure: the respawned process
	// keeps its fresh launch and receives the credential.
	eng.exitAfterLookup = true
	race, raceLaunch := spec()
	in, err := d.ensurePane(context.Background(), race)
	if err != nil || in == running || !pending(raceLaunch) {
		t.Fatalf("respawn after exit lost its launch: %v", err)
	}
	if err := accept(raceLaunch); err != nil {
		t.Fatal("respawned pane refused its fresh launch", err)
	}
	// A failed spawn started nothing.
	eng.ensureErr = errors.New("spawn failed")
	eng.mu.Lock()
	delete(eng.insts, key)
	eng.mu.Unlock()
	failed, failedLaunch := spec()
	if _, err := d.ensurePane(context.Background(), failed); err == nil || pending(failedLaunch) {
		t.Fatalf("failed spawn kept its launch: %v", err)
	}
}
