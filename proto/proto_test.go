package proto

import (
	"encoding/json"
	"net"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonKeys marshals v and returns its top-level JSON object keys, sorted. v must
// have every field set to a non-zero value so omitempty fields are present.
func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", v, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestGoldenJSONKeys locks the wire format: the exact JSON key of every field on
// every wire type. A rename, retag, added, or removed field breaks this test, so
// byte-compatibility with external orchestrators cannot regress silently. The
// golden sets below ARE the contract — update them only alongside a deliberate,
// documented protocol change.
func TestGoldenJSONKeys(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want []string
	}{
		{
			name: "MuxMsg",
			val: MuxMsg{
				Type: "x", Version: 1, PaneID: "p", Dir: "/d", Env: []string{"K=V"},
				Argv: []string{"sh"}, Cols: 80, Rows: 24, Data: []byte("d"),
				OK: true, Error: "e", ProviderID: "pr", HeartbeatSeconds: 15, GraceSeconds: 60,
				Adopt: []AdoptPane{{PaneID: "p"}}, Kill: []string{"p"}, T: 1,
				ReqID: "r", Action: "a", ID: "i", Target: "t", Fields: map[string]string{"k": "v"},
				SessionID: "s", AfterSeq: 1,
			},
			want: []string{
				"action", "adopt", "afterSeq", "argv", "cols", "data", "dir", "env",
				"error", "fields", "graceSeconds", "heartbeatSeconds", "id", "kill",
				"ok", "paneId", "providerId", "reqId", "rows", "sessionId", "t",
				"target", "type", "version",
			},
		},
		{
			name: "HarnessMsg",
			val: HarnessMsg{
				Type: "x", Version: 1, PaneID: "p", Data: []byte("d"), Error: "e",
				Seq: 1, Versions: []int{1, 2}, Token: "tok", Name: "n",
				Labels: map[string]string{"k": "v"}, Capabilities: &Capabilities{MaxPanes: 1},
				Panes: []PaneOffer{{PaneID: "p"}}, T: 1,
				Sessions: []Session{{ID: "s"}}, ReqID: "r", OK: true, NewID: "nid",
				SessionID: "s", Events: []RuntimeEvent{{Type: "t"}},
			},
			want: []string{
				"capabilities", "data", "error", "events", "labels", "name", "newId",
				"ok", "paneId", "panes", "reqId", "seq", "sessionId", "sessions",
				"t", "token", "type", "version", "versions",
			},
		},
		{
			name: "Capabilities",
			val:  Capabilities{MaxPanes: 1, Bwrap: true, OS: "linux", Arch: "amd64", Features: []string{"pty"}},
			want: []string{"arch", "bwrap", "features", "maxPanes", "os"},
		},
		{
			name: "PaneOffer",
			val:  PaneOffer{PaneID: "p", OutSeq: 1, Running: true},
			want: []string{"outSeq", "paneId", "running"},
		},
		{
			name: "AdoptPane",
			val:  AdoptPane{PaneID: "p", AfterSeq: 1},
			want: []string{"afterSeq", "paneId"},
		},
		{
			name: "RuntimeEvent",
			val:  RuntimeEvent{Type: "t", ItemID: "i", Direction: "d", Payload: json.RawMessage(`{}`)},
			want: []string{"direction", "item_id", "payload", "type"},
		},
		{
			name: "Session",
			val: Session{
				ID: "i", Title: "t", Source: "s", Kind: "k", Mode: "m", RootID: "r",
				IsRoot: true, Repos: "a,b", Section: "sec", State: "running", Status: "st",
				Archived: true, Cwd: "/c", Pid: 1, StartedAt: 1, CanAttach: true,
				CanKill: true, CanResume: true,
			},
			want: []string{
				"archived", "canAttach", "canKill", "canResume", "cwd", "id", "isRoot",
				"kind", "mode", "pid", "repos", "rootId", "section", "source",
				"startedAt", "state", "status", "title",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsonKeys(t, c.val)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s JSON keys drifted:\n got  %s\n want %s",
					c.name, strings.Join(got, ","), strings.Join(c.want, ","))
			}
		})
	}
}

// TestCodecRoundTrip exercises every message type over a real net.Pipe: the
// provider (harness side) sends register/output/reset/ping/exit plus the sessions
// and runtime-events frames; the orchestrator (mux side) sends registered/pong/
// spawn plus subscribe and session-action. Each must decode on the other end
// byte-for-byte, including the additive v2 fields.
func TestCodecRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	prov := NewConn(a) // provider: writes HarnessMsg, reads MuxMsg
	orch := NewConn(b) // orchestrator: writes MuxMsg, reads HarnessMsg
	defer prov.Close()
	defer orch.Close()

	harnessMsgs := []HarnessMsg{
		{
			Type:     HRegister,
			Versions: []int{1, 2},
			Token:    "s3cr3t",
			Name:     "mybox",
			Labels:   map[string]string{"zone": "home", "gpu": "none"},
			Capabilities: &Capabilities{
				MaxPanes: 8, Bwrap: true, OS: "linux", Arch: "amd64",
				Features: []string{SessionsFeature, RuntimeEventsFeature},
			},
			Panes: []PaneOffer{{PaneID: "p1", OutSeq: 42, Running: true}},
		},
		{Type: HOutput, PaneID: "p1", Data: []byte("hello \x1b[31mworld\x1b[0m"), Seq: 43},
		{Type: HReset, PaneID: "p1", Seq: 44},
		{Type: HPing, T: 1720000000},
		{Type: HExit, PaneID: "p1", Error: "signal: killed", Seq: 45},
		{Type: HSessions, Sessions: []Session{{ID: "s1", Title: "one", Status: "ready", Cwd: "/w"}}, Seq: 1},
		{Type: HSessionResult, ReqID: "r1", OK: true, NewID: "s2"},
		{Type: HRuntimeEvents, SessionID: "s1", Seq: 7, Events: []RuntimeEvent{
			{Type: "message", ItemID: "m1", Direction: "out", Payload: json.RawMessage(`{"text":"hi"}`)},
		}},
	}
	muxMsgs := []MuxMsg{
		{
			Type: MRegistered, OK: true, Version: 2, ProviderID: "prov-7",
			HeartbeatSeconds: 15, GraceSeconds: 60,
			Adopt: []AdoptPane{{PaneID: "p1", AfterSeq: 42}},
			Kill:  []string{"p9"},
		},
		{Type: MSpawn, PaneID: "p2", Dir: "/tmp", Env: []string{"K=V"}, Argv: []string{"sh", "-c", "echo hi"}, Cols: 80, Rows: 24},
		{Type: MPong, T: 1720000000},
		{Type: MSessionsSubscribe},
		{Type: MSessionAction, ReqID: "r1", Action: VerbRename, ID: "s1", Fields: map[string]string{"name": "new"}},
		{Type: MRuntimeEventsSubscribe, SessionID: "s1", AfterSeq: 3},
	}

	// net.Pipe is unbuffered, so each write must run concurrently with its read.
	for _, want := range harnessMsgs {
		go func() { _ = prov.WriteHarness(want) }()
		got, err := orch.ReadHarness()
		if err != nil {
			t.Fatalf("read %s: %v", want.Type, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s round-trip mismatch:\n got  %+v\n want %+v", want.Type, got, want)
		}
	}
	for _, want := range muxMsgs {
		go func() { _ = orch.WriteMux(want) }()
		got, err := prov.ReadMux()
		if err != nil {
			t.Fatalf("read %s: %v", want.Type, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s round-trip mismatch:\n got  %+v\n want %+v", want.Type, got, want)
		}
	}
}

func TestTokenOK(t *testing.T) {
	cases := []struct {
		configured, presented string
		want                  bool
	}{
		{"", "", true},
		{"", "anything", true},
		{"secret", "secret", true},
		{"secret", "wrong", false},
		{"secret", "", false},
		{"secret", "secre", false},
		{"secret", "secrett", false},
	}
	for _, c := range cases {
		if got := TokenOK(c.configured, c.presented); got != c.want {
			t.Errorf("TokenOK(%q,%q)=%v want %v", c.configured, c.presented, got, c.want)
		}
	}
}

func TestNegotiate(t *testing.T) {
	cases := []struct {
		offered, supported []int
		wantV              int
		wantOK             bool
	}{
		{[]int{1, 2}, []int{1, 2}, 2, true},
		{[]int{1, 2}, []int{1}, 1, true},
		{[]int{2}, []int{1}, 0, false},
		{[]int{1}, []int{2}, 0, false},
		{[]int{2, 1}, []int{1, 2}, 2, true},
		{nil, []int{1, 2}, 0, false},
	}
	for _, c := range cases {
		v, ok := Negotiate(c.offered, c.supported)
		if v != c.wantV || ok != c.wantOK {
			t.Errorf("Negotiate(%v,%v)=(%d,%v) want (%d,%v)", c.offered, c.supported, v, ok, c.wantV, c.wantOK)
		}
	}
}
