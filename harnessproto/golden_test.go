package harnessproto

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// update regenerates the golden fixtures: `go test ./harnessproto -run Golden -update`.
// Regenerating is a deliberate act — a reviewer sees the fixture diff in the PR,
// which is exactly the drift signal we want.
var update = flag.Bool("update", false, "regenerate golden wire fixtures")

// goldenFrames is one canonical value per wire message type whose JSON tags are
// load-bearing (they are what an external consumer — the harness orchestrator —
// decodes). The frames are chosen to exercise every mirrored struct: MuxMsg,
// HarnessMsg, Capabilities, PaneOffer, AdoptPane, Session, and RuntimeEvent.
//
// TestGoldenFrames marshals each value and compares it byte-for-byte against the
// checked-in fixture, then decodes the fixture back and asserts it re-marshals
// identically. A JSON-tag rename (or a field add/remove/reorder) changes the
// bytes and fails here — so wire drift breaks PR CI in the repo that owns the
// protocol, satisfying AGE-130's acceptance criterion.
var goldenFrames = []struct {
	name string
	msg  any
}{
	{"register", HarnessMsg{
		Type:     HRegister,
		Versions: []int{1, 2},
		Token:    "hp_token",
		Name:     "mybox",
		Labels:   map[string]string{"os": "linux", "zone": "home"},
		Capabilities: &Capabilities{
			MaxPanes: 8, Bwrap: true, OS: "linux", Arch: "amd64", Features: []string{"pane-seq", "sessions", "runtime-events"},
		},
		Panes: []PaneOffer{{PaneID: "p1", OutSeq: 42, Running: true}},
	}},
	{"registered", MuxMsg{
		Type: MRegistered, OK: true, Version: 2, ProviderID: "prov-7",
		HeartbeatSeconds: 15, GraceSeconds: 60,
		Adopt: []AdoptPane{{PaneID: "p1", AfterSeq: 42}},
		Kill:  []string{"p9"},
	}},
	{"spawn", MuxMsg{
		Type: MSpawn, PaneID: "p2", Dir: "/tmp", Env: []string{"K=V"},
		Argv: []string{"sh", "-c", "echo hi"}, Cols: 80, Rows: 24,
	}},
	{"output", HarnessMsg{Type: HOutput, PaneID: "p1", Data: []byte("hello \x1b[31mworld\x1b[0m"), Seq: 43}},
	{"session_action", MuxMsg{
		Type: MSessionAction, ReqID: "r1", Action: VerbAddAgent, ID: "wg1",
		Fields: map[string]string{"kind": "claude"},
	}},
	{"session_action_prompt", MuxMsg{
		Type: MSessionAction, ReqID: "r2", Action: VerbPrompt, ID: "a2",
		Fields: map[string]string{FieldText: "run the tests"},
	}},
	{"session_action_permission", MuxMsg{
		Type: MSessionAction, ReqID: "r3", Action: VerbPermission, ID: "a2",
		Fields: map[string]string{
			FieldRequestID: "perm-9", FieldDecision: DecisionDeny,
			FieldReason: "writes outside the worktree",
		},
	}},
	{"session_result_accepted", HarnessMsg{
		Type: HSessionResult, ReqID: "r2", OK: true, Result: ResultAccepted, Accepted: true,
	}},
	{"sessions", HarnessMsg{
		Type: HSessions,
		Sessions: []Session{{
			ID: "s1", Title: "fix the bug", Source: "claude", Kind: "claude", Mode: "task",
			RootID: "wg1", Repos: "amux", Section: SectionWorkgroups, State: StateWaiting,
			Status: "waiting · main", Cwd: "/home/u/amux", StartedAt: 1720000000,
			CanAttach: true, CanKill: true,
		}},
	}},
	{"sessions_caps", HarnessMsg{
		Type: HSessions,
		Sessions: []Session{{
			ID: "c1", Title: "codex fix", Source: "workspace", Kind: RuntimeCodex, Mode: "task",
			Section: SectionRepos, State: StateRunning, Status: "running · api",
			Cwd: "/home/u/api", StartedAt: 1720000001, CanAttach: true, CanKill: true,
			Runtime: RuntimeCodex,
			Caps:    &SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true},
		}},
	}},
	{"runtime_events", HarnessMsg{
		Type: HRuntimeEvents, SessionID: "s1", Runtime: RuntimeClaude, Seq: 7,
		Events: []RuntimeEvent{
			{Type: TypeTurnStart, Direction: DirOut, Payload: json.RawMessage(`{}`)},
			{Type: TypeToolCall, ItemID: "t1", Direction: DirOut, Payload: json.RawMessage(`{"title":"Read"}`)},
		},
	}},
}

func TestGoldenFrames(t *testing.T) {
	for _, f := range goldenFrames {
		t.Run(f.name, func(t *testing.T) {
			path := filepath.Join("testdata", f.name+".json")

			got, err := json.Marshal(f.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got = append(got, '\n')

			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("wire drift for %s:\n got:  %s want: %s\nA JSON-tag or field change altered the wire. If intentional, run: go test ./harnessproto -run Golden -update",
					f.name, got, want)
			}

			// The fixture must also decode back into the same value shape and
			// re-marshal identically (guards a rename a lenient decode would tolerate).
			out := reflect.New(reflect.TypeOf(f.msg)).Interface()
			if err := json.Unmarshal(want, out); err != nil {
				t.Fatalf("unmarshal golden: %v", err)
			}
			reencoded, err := json.Marshal(reflect.ValueOf(out).Elem().Interface())
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			reencoded = append(reencoded, '\n')
			if !bytes.Equal(reencoded, want) {
				t.Fatalf("re-encode mismatch for %s:\n got:  %s want: %s", f.name, reencoded, want)
			}
		})
	}
}
