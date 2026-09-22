package provider

import (
	"context"
	"net"
	"reflect"
	"testing"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The goal verb is the user's control over a goal session's native goal —
// pause, resume, complete, clear, raise the budget. Those are decisions the
// runtime and the agent never make, so a consumer that can only reach amux over
// the wire (the web dashboard, a remote orchestrator) needs the verb to offer
// the same control a host terminal has with `amux do steer <id> -f verb=goal`.
// These tests pin the wire spelling, the mapping onto the daemon's own steer
// action, and the shape check that rejects a malformed control before it costs
// a round-trip.

// TestGoalVerbSpellingMatchesDaemon pins the wire vocabulary to the daemon's.
// steerFields copies the wire keys through verbatim, so a divergent spelling
// would not fail to compile — it would silently deliver an empty field.
func TestGoalVerbSpellingMatchesDaemon(t *testing.T) {
	for _, pair := range []struct{ wire, daemon, what string }{
		{harnessproto.VerbGoal, core.SteerGoal, "verb"},
		{harnessproto.FieldGoalStatus, core.SteerGoalStatus, "status field"},
		{harnessproto.FieldGoalObjective, core.SteerGoalObjective, "objective field"},
		{harnessproto.FieldGoalBudget, core.SteerGoalBudget, "budget field"},
	} {
		if pair.wire != pair.daemon {
			t.Errorf("%s: wire %q != daemon %q", pair.what, pair.wire, pair.daemon)
		}
	}
	if !harnessproto.SessionVerbs[harnessproto.VerbGoal] || !harnessproto.SteeringVerbs[harnessproto.VerbGoal] {
		t.Error("goal is not an accepted steering verb")
	}
}

// TestGoalVerbRoutesToSteer walks the full host-to-provider path for each
// control a user can send, and asserts the daemon receives exactly the steer
// action `amux do steer` would have built locally — no more fields, no fewer.
func TestGoalVerbRoutesToSteer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   map[string]string
	}{
		{
			name:   "pause",
			fields: map[string]string{harnessproto.FieldGoalStatus: harnessproto.GoalPaused},
			want:   map[string]string{core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: harnessproto.GoalPaused},
		},
		{
			name:   "resume",
			fields: map[string]string{harnessproto.FieldGoalStatus: harnessproto.GoalActive},
			want:   map[string]string{core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: harnessproto.GoalActive},
		},
		{
			name:   "clear",
			fields: map[string]string{harnessproto.FieldGoalStatus: harnessproto.GoalClear},
			want:   map[string]string{core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: harnessproto.GoalClear},
		},
		{
			name: "raise the budget while resuming",
			fields: map[string]string{
				harnessproto.FieldGoalStatus: harnessproto.GoalActive,
				harnessproto.FieldGoalBudget: "250000",
			},
			want: map[string]string{
				core.SteerVerb:       core.SteerGoal,
				core.SteerGoalStatus: harnessproto.GoalActive,
				core.SteerGoalBudget: "250000",
			},
		},
		{
			name: "replace the objective, unknown fields dropped",
			fields: map[string]string{
				harnessproto.FieldGoalObjective: "ship the release branch",
				"nosuchfield":                   "dropped",
			},
			want: map[string]string{
				core.SteerVerb:          core.SteerGoal,
				core.SteerGoalObjective: "ship the release branch",
			},
		},
		{
			name: "complete",
			fields: map[string]string{
				harnessproto.FieldGoalStatus: harnessproto.GoalComplete,
			},
			want: map[string]string{core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: harnessproto.GoalComplete},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, res := sendGoalVerb(t, "wg1", tc.fields)
			if !res.OK {
				t.Fatalf("result = %+v, want ok", res)
			}
			// A goal control is taken on, not completed: Codex continues an
			// activated goal on its own once idle, so the reply must not claim the
			// effect is already observable in the next snapshot.
			if res.Result != harnessproto.ResultAccepted || !res.Accepted {
				t.Fatalf("result disposition = %q accepted=%v, want %q", res.Result, res.Accepted, harnessproto.ResultAccepted)
			}
			want := core.Action{Action: core.ActionSteer, ID: "wg1", Fields: tc.want}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("apply got %+v, want %+v", got, want)
			}
		})
	}
}

// TestMalformedGoalVerbRejectedBeforeTheDaemon covers every shape the wire can
// answer for itself. Each one must be refused with a message naming the field,
// and must never reach ApplyAction: a bad spelling relayed onward would be
// answered by the runtime — or, worse, half-honored.
func TestMalformedGoalVerbRejectedBeforeTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]string
	}{
		{"unknown status", map[string]string{harnessproto.FieldGoalStatus: "resumed"}},
		{"runtime-only status", map[string]string{harnessproto.FieldGoalStatus: "blocked"}},
		{"status case mismatch", map[string]string{harnessproto.FieldGoalStatus: "Paused"}},
		{"budget not a number", map[string]string{harnessproto.FieldGoalBudget: "lots"}},
		{"budget zero", map[string]string{harnessproto.FieldGoalBudget: "0"}},
		{"budget negative", map[string]string{harnessproto.FieldGoalBudget: "-5"}},
		{"budget fractional", map[string]string{harnessproto.FieldGoalBudget: "1.5"}},
		{"clear with an objective", map[string]string{
			harnessproto.FieldGoalStatus:    harnessproto.GoalClear,
			harnessproto.FieldGoalObjective: "something else",
		}},
		{"clear with a budget", map[string]string{
			harnessproto.FieldGoalStatus: harnessproto.GoalClear,
			harnessproto.FieldGoalBudget: "1000",
		}},
		{"nothing to set", map[string]string{}},
		{"only blank values", map[string]string{
			harnessproto.FieldGoalStatus:    "   ",
			harnessproto.FieldGoalObjective: " ",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingApply{}
			p := New(Config{PublishSessions: true, Sessions: emptySessions, ApplyAction: rec.apply})
			_, err := p.applySessionAction(context.Background(), harnessproto.MuxMsg{
				Type: harnessproto.MSessionAction, Action: harnessproto.VerbGoal, ID: "wg1", Fields: tc.fields,
			})
			if err == nil {
				t.Fatal("malformed goal control was accepted")
			}
			if _, called := rec.last(); called {
				t.Fatalf("malformed goal control reached the daemon: %v", err)
			}
		})
	}
}

// TestGoalVerbRequiresPublishedTarget: the goal verb is a targeted verb like any
// other steering verb, so it is bound to the connection's current inventory. It
// must not inherit new-workgroup's exemption from naming a live target.
func TestGoalVerbRequiresPublishedTarget(t *testing.T) {
	rec := &recordingApply{}
	p := New(Config{PublishSessions: true, Sessions: emptySessions, ApplyAction: rec.apply})
	s := &session{published: map[string]core.Session{}, actions: map[uint64]*sessionActionCall{}}
	s.rtCtx = context.Background()
	if _, err := p.applyAuthorizedSessionAction(s, harnessproto.MuxMsg{
		Action: harnessproto.VerbGoal, ID: "", Fields: map[string]string{harnessproto.FieldGoalStatus: harnessproto.GoalPaused},
	}); err == nil {
		t.Fatal("goal control with no target was accepted")
	}
	if _, called := rec.last(); called {
		t.Fatal("untargeted goal control reached the daemon")
	}
}

// TestReadOnlyRejectsGoalVerb: read-only publishing is inventory only. The goal
// verb mutates the session's work, so it is refused exactly as the other
// steering verbs are.
func TestReadOnlyRejectsGoalVerb(t *testing.T) {
	rec := &recordingApply{}
	p := New(Config{
		PublishSessions: true, Sessions: emptySessions, ApplyAction: rec.apply, ReadOnlySessions: true,
	})
	if _, err := p.applySessionAction(context.Background(), harnessproto.MuxMsg{
		Action: harnessproto.VerbGoal, ID: "wg1", Fields: map[string]string{harnessproto.FieldGoalStatus: harnessproto.GoalPaused},
	}); err == nil {
		t.Fatal("read-only provider accepted a goal control")
	}
	if _, called := rec.last(); called {
		t.Fatal("read-only provider relayed a goal control")
	}
}

// TestPublishedGoalCapabilityIsHonest: the caps a provider publishes decide
// which sessions a consumer offers the control on. Only a session actually
// under native goal supervision may advertise it — an ordinary agent on the
// same runtime must not, or the consumer would offer a control the daemon
// refuses.
func TestPublishedGoalCapabilityIsHonest(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		role string
		want bool
	}{
		{"goal coordinator", agent.GoalRuntime, harnessproto.RoleCoordinator, true},
		{"coordinator on another harness", "claude", harnessproto.RoleCoordinator, false},
		{"ordinary agent on the goal runtime", agent.GoalRuntime, "", false},
		{"repo home on the goal runtime", agent.GoalRuntime, harnessproto.RoleRepo, false},
		{"console", agent.GoalRuntime, harnessproto.RoleConsole, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := agent.CapsForRole(tc.kind, tc.role).Goal; got != tc.want {
				t.Fatalf("Goal cap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGoalCapabilityMatchesNativeGoals pins the published advertisement to the
// daemon's own reading of the same session. These are two expressions of one
// fact; if they drift, a consumer offers (or hides) the control wrongly.
func TestGoalCapabilityMatchesNativeGoals(t *testing.T) {
	for _, kind := range append(agent.Kinds(), "") {
		for _, scope := range []string{store.ScopeWork, store.ScopeRepo} {
			for _, rootID := range []string{"", "wg1"} {
				s := store.Session{ID: "s1", RootID: rootID, Scope: scope, Agent: kind}
				want := agent.NativeGoals(s)
				if got := agent.CapsForRole(kind, s.Role()).Goal; got != want {
					t.Fatalf("kind=%q scope=%q root=%q: cap %v != NativeGoals %v", kind, scope, rootID, got, want)
				}
			}
		}
	}
}

func emptySessions(context.Context) ([]core.Session, error) { return nil, nil }

// sendGoalVerb drives one goal control over a real connection — register,
// subscribe, publish a goal-capable session, then send the verb — and returns
// the action the daemon was handed with the wire result.
func sendGoalVerb(t *testing.T, id string, fields map[string]string) (core.Action, harnessproto.HarnessMsg) {
	t.Helper()
	conns := make(chan net.Conn, 1)
	rec := &recordingApply{}
	src := &mutableSource{}
	p := newFast(Config{
		Orchestrator: "pipe", Dial: pipeDialer(conns),
		PublishSessions: true, Sessions: src.poll, ApplyAction: rec.apply,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	oc := harnessproto.NewConn(<-conns)
	accept(t, oc, 2, nil, 60)
	caps := agent.CapsForRole(agent.GoalRuntime, harnessproto.RoleCoordinator)
	if !caps.Goal {
		t.Fatal("fixture session does not advertise the goal control")
	}
	publishRows(t, oc, src, core.Session{
		ID: id, Role: harnessproto.RoleCoordinator, Kind: agent.GoalRuntime, Caps: &caps,
	})
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSessionAction, ReqID: "g1", Action: harnessproto.VerbGoal, ID: id, Fields: fields,
	}); err != nil {
		t.Fatal(err)
	}
	res := readResult(t, oc)
	if res.ReqID != "g1" {
		t.Fatalf("result = %+v, want the goal request", res)
	}
	got, called := rec.last()
	if !called {
		t.Fatalf("goal control never reached ApplyAction: %+v", res)
	}
	return got, res
}
