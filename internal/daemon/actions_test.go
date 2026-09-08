package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/store"
	"amux/internal/wsops"
)

func TestAuthenticatedHostRestoreExplicitlyRegrantsRevokedCredential(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	d.sessionRPC = runtime
	t.Cleanup(runtime.completions.close)
	eng := newFakeEngine()
	d.engine = eng
	oldRuntime := eng.running(session.ID)
	if _, err := d.permissions.observe(session.ID, oldRuntime); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(session.ID, true, time.Now().UnixMilli()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.authority.RevokeCurrent(context.Background(), principals[session.ID]); err != nil {
		t.Fatal(err)
	}
	terminatedBeforeRegrant := false
	eng.killObserved = func(instance engine.Instance) {
		if instance != oldRuntime || instance.Alive() {
			t.Errorf("restore kill did not terminate the captured runtime")
			return
		}
		if _, err := d.authority.Current(context.Background(), access.SubjectSession, session.ID); !errors.Is(err, access.ErrRevoked) {
			t.Errorf("successor credential published before runtime termination: %v", err)
			return
		}
		terminatedBeforeRegrant = true
	}

	result := d.handle(context.Background(), core.Action{
		Action: core.ActionSetArchived, ID: session.ID,
		Fields: map[string]string{"archived": "false"},
	})
	if !result.OK {
		t.Fatalf("restore = %+v", result)
	}
	if !terminatedBeforeRegrant {
		t.Fatal("restore did not prove runtime termination before regrant")
	}
	if err := d.authority.Valid(context.Background(), principals[session.ID]); err == nil {
		t.Fatal("old principal remained valid after restore")
	}
	current, err := d.authority.Current(context.Background(), access.SubjectSession, session.ID)
	if err != nil || current.Generation != principals[session.ID].Generation+1 {
		t.Fatalf("regranted current = %+v, err=%v", current, err)
	}
	restored, found, err := lookupSession(session.ID)
	if err != nil || !found || restored.Archived {
		t.Fatalf("restored session = %+v, found=%v, err=%v", restored, found, err)
	}
}

func TestAuthenticatedHostRestoreDrainsPendingCompletion(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	d.sessionRPC = runtime
	t.Cleanup(runtime.completions.close)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(session.ID, true, time.Now().UnixMilli()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.completions.minimum = time.Hour
	runtime.completions.begin(principals[session.ID], "0123456789abcdef0123456789abcdef", session.ID)

	result := d.handle(context.Background(), core.Action{Action: core.ActionArchive, ID: session.ID})
	if !result.OK {
		t.Fatalf("toggle restore = %+v", result)
	}
	if runtime.completions.has(session.ID) {
		t.Fatal("restore retained stale completion ownership")
	}
	if err := d.authority.Valid(context.Background(), principals[session.ID]); err != nil {
		t.Fatalf("drained completion revoked current principal: %v", err)
	}
}

func TestAuthenticatedHostRestoreFailsBeforeRegrantWithoutRuntimeQuiescence(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	d.sessionRPC = runtime
	t.Cleanup(runtime.completions.close)
	eng := newFakeEngine()
	eng.killRefuses = true
	d.engine = eng
	oldRuntime := eng.running(session.ID)
	if _, err := d.permissions.observe(session.ID, oldRuntime); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(session.ID, true, time.Now().UnixMilli()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.authority.RevokeCurrent(context.Background(), principals[session.ID]); err != nil {
		t.Fatal(err)
	}

	result := d.handle(context.Background(), core.Action{
		Action: core.ActionSetArchived, ID: session.ID,
		Fields: map[string]string{"archived": "false"},
	})
	if result.OK || !strings.Contains(result.Error, "did not terminate") {
		t.Fatalf("restore without quiescence = %+v", result)
	}
	if current, found, err := lookupSession(session.ID); err != nil || !found || !current.Archived {
		t.Fatalf("failed restore changed archive state: %+v, found=%v, err=%v", current, found, err)
	}
	if _, err := d.authority.Current(context.Background(), access.SubjectSession, session.ID); !errors.Is(err, access.ErrRevoked) {
		t.Fatalf("failed restore published successor credential: %v", err)
	}
}

func TestAuthenticatedShutdownAcknowledgesBeforeStoppingRun(t *testing.T) {
	d := New("/amux", nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	result := d.handle(ctx, core.Action{Action: actionDaemonShutdown})
	if !result.OK {
		t.Fatalf("shutdown result = %+v", result)
	}
	// A real client closes after reading the acknowledgement. That must not
	// retract an operation the authenticated daemon already accepted.
	cancel()
	select {
	case <-d.shutdown:
		t.Fatal("shutdown began before the authenticated response grace")
	default:
	}
	select {
	case <-d.shutdown:
	case <-time.After(2 * shutdownResponseGrace):
		t.Fatal("shutdown was not requested after response grace")
	}
}

func TestAuthenticatedShutdownRejectsSmuggledArguments(t *testing.T) {
	d := New("/amux", nil, time.Second)
	result := d.handle(context.Background(), core.Action{Action: actionDaemonShutdown, ID: "other"})
	if result.OK {
		t.Fatalf("shutdown with target succeeded: %+v", result)
	}
	select {
	case <-d.shutdown:
		t.Fatal("rejected shutdown stopped daemon")
	default:
	}
}

func TestHostSessionRecreationPreflightsBeforeStoppingAndRotatesRuntimeGeneration(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	original := eng.running("a1")
	firstGeneration, err := d.permissions.observe("a1", original)
	if err != nil {
		t.Fatal(err)
	}
	d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, nil, fmt.Errorf("unsupported legacy layout")
	}
	if result := d.handle(context.Background(), core.Action{Action: actionSessionRecreate, ID: "a1"}); result.OK {
		t.Fatalf("recreation unexpectedly succeeded: %+v", result)
	}
	if got, ok := eng.Lookup(original.Key()); !ok || got != original {
		t.Fatal("failed recreation stopped the existing runtime")
	}
	d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, []string{"sh"}, nil
	}
	if result := d.handle(context.Background(), core.Action{Action: actionSessionRecreate, ID: "a1"}); !result.OK {
		t.Fatalf("recreation failed: %+v", result)
	}
	replacement, ok := eng.Lookup(original.Key())
	if !ok || replacement == original {
		t.Fatal("recreation did not replace the named runtime")
	}
	secondGeneration, ok := d.permissions.generation("a1")
	if !ok || secondGeneration == firstGeneration {
		t.Fatal("recreated runtime reused stale permission generation")
	}
}

func TestHostSessionRecreationRejectsSmuggledFields(t *testing.T) {
	d := New("", nil, time.Hour)
	result := d.handle(context.Background(), core.Action{Action: actionSessionRecreate, ID: "a1", Fields: map[string]string{"target": "other"}})
	if result.OK || !strings.Contains(result.Error, "exactly one session id") {
		t.Fatalf("smuggled recreation = %+v", result)
	}
}

// Creation must launch the coordinator without any pane.open or follow-up start
// action. New-workgroup configures that default session directly; the older
// create-workspace verb can still explicitly request a child agent.
func TestCreateWorkgroupStartsCoordinator(t *testing.T) {
	for _, action := range []string{core.ActionNewWorkgroup, core.ActionCreateWorkspace} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/configured=%v", action, configured), func(t *testing.T) {
				d, eng := steerDaemon(t)
				fields := map[string]string{"name": "payments", "agent": "claude"}
				if configured {
					fields["prompt"] = "fix the idempotency bug"
					if action == core.ActionCreateWorkspace {
						fields["defaultAgent"] = "1"
					}
				}
				r := d.handle(context.Background(), core.Action{Action: action, Fields: fields})
				if !r.OK || r.NewID == "" {
					t.Fatalf("create: %+v", r)
				}
				key := engine.Key{AgentID: r.NewID, Tab: panespec.TabAgent}
				inst, ok := eng.Lookup(key)
				if !ok || !inst.Alive() {
					t.Fatal("coordinator is not running after creation")
				}
				if keys := eng.ensuredKeys(); len(keys) != 1 || keys[0] != key {
					t.Fatalf("creation launched %v, want just the coordinator %v", keys, key)
				}

				db, err := store.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				root, ok, err := db.GetSession(r.NewID)
				if err != nil || !ok || root.Role() != store.RoleCoordinator {
					t.Fatalf("coordinator row: %+v, %v", root, err)
				}
				wantRootPrompt := ""
				if action == core.ActionNewWorkgroup && configured {
					wantRootPrompt = fields["prompt"]
				}
				if root.Mode != store.ModeInteractive || root.Prompt != wantRootPrompt {
					t.Fatalf("coordinator config = mode %q, prompt %q; want %q, %q", root.Mode, root.Prompt, store.ModeInteractive, wantRootPrompt)
				}
				kids, err := db.Children(r.NewID)
				if err != nil {
					t.Fatal(err)
				}
				if action == core.ActionCreateWorkspace && configured {
					if len(kids) != 1 || kids[0].Prompt != fields["prompt"] {
						t.Fatalf("first member lost its creation prompt: %+v", kids)
					}
				} else if len(kids) != 0 {
					t.Fatalf("workgroup unexpectedly has members: %+v", kids)
				}
			})
		}
	}
}

// A remote client receives the created id but does not attach a pane the way
// the native TUI does. Adding an agent must therefore start its process as part
// of the daemon action, or its persisted creation prompt waits until somebody
// manually opens the session.
func TestAddAgentStartsWithoutClientAttach(t *testing.T) {
	d, eng := steerDaemon(t)
	rootID, err := wsops.CreateWorkspace(context.Background(), "payments", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := d.handle(context.Background(), core.Action{
		Action: core.ActionAddAgent,
		ID:     rootID,
		Fields: map[string]string{"agent": "claude", "prompt": "fix the Android flow"},
	})
	if !r.OK || r.NewID == "" {
		t.Fatalf("add agent: %+v", r)
	}
	key := engine.Key{AgentID: r.NewID, Tab: panespec.TabAgent}
	inst, ok := eng.Lookup(key)
	if !ok || !inst.Alive() {
		t.Fatal("created agent is not running after the action")
	}
	if keys := eng.ensuredKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("creation launched %v, want just the new agent %v", keys, key)
	}
}

func TestCreateWorkgroupReportsCoordinatorLaunchFailure(t *testing.T) {
	d, eng := steerDaemon(t)
	eng.ensureErr = fmt.Errorf("runtime unavailable")
	r := d.handle(context.Background(), core.Action{
		Action: core.ActionNewWorkgroup, Fields: map[string]string{"name": "payments"},
	})
	if r.OK || r.NewID == "" || !strings.Contains(r.Error, "workgroup "+r.NewID+" created") ||
		!strings.Contains(r.Error, "runtime unavailable") || !strings.Contains(r.Error, "retry") {
		t.Fatalf("launch failure should identify the created workgroup and allow retry: %+v", r)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, err := db.GetSession(r.NewID); err != nil || !ok {
		t.Fatalf("created workgroup missing after launch failure: %v", err)
	}
	eng.ensureErr = nil
	if err := d.startAgent(context.Background(), r.NewID); err != nil {
		t.Fatalf("retry starting coordinator: %v", err)
	}
}

func TestFailedWorkgroupCreationDoesNotLaunchCoordinator(t *testing.T) {
	d, eng := steerDaemon(t)
	r := d.handle(context.Background(), core.Action{
		Action: core.ActionNewWorkgroup,
		Fields: map[string]string{"prompt": "go", "agent": "unknown-harness"},
	})
	if r.OK {
		t.Fatal("creation with an invalid harness succeeded")
	}
	if keys := eng.ensuredKeys(); len(keys) != 0 {
		t.Fatalf("failed creation launched sessions: %v", keys)
	}
}
