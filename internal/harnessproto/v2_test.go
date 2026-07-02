package harnessproto

import (
	"net"
	"reflect"
	"testing"
)

// TestV2CodecRoundTrip exercises every v2 message type over a real net.Pipe: the
// provider (harness side) sends register/output/reset/ping; the orchestrator
// (mux side) sends registered/pong. Each must decode on the other end byte-for-
// byte, including the additive v2 fields (seq, caps, adopt/kill, timestamps).
func TestV2CodecRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	prov := NewConn(a) // provider: writes HarnessMsg, reads MuxMsg
	orch := NewConn(b) // orchestrator: writes MuxMsg, reads HarnessMsg
	defer prov.Close()
	defer orch.Close()

	// Provider -> orchestrator frames.
	harnessMsgs := []HarnessMsg{
		{
			Type:     HRegister,
			Versions: []int{1, 2},
			Token:    "s3cr3t",
			Name:     "mybox",
			Labels:   map[string]string{"zone": "home", "gpu": "none"},
			Capabilities: &Capabilities{
				MaxPanes: 8, Bwrap: true, OS: "linux", Arch: "amd64", Features: []string{"pty"},
			},
			Panes: []PaneOffer{{PaneID: "p1", OutSeq: 42, Running: true}},
		},
		{Type: HOutput, PaneID: "p1", Data: []byte("hello \x1b[31mworld\x1b[0m"), Seq: 43},
		{Type: HReset, PaneID: "p1", Seq: 44},
		{Type: HPing, T: 1720000000},
		{Type: HExit, PaneID: "p1", Error: "signal: killed", Seq: 45},
	}
	// Orchestrator -> provider frames.
	muxMsgs := []MuxMsg{
		{
			Type: MRegistered, OK: true, Version: 2, ProviderID: "prov-7",
			HeartbeatSeconds: 15, GraceSeconds: 60,
			Adopt: []AdoptPane{{PaneID: "p1", AfterSeq: 42}},
			Kill:  []string{"p9"},
		},
		{Type: MSpawn, PaneID: "p2", Dir: "/tmp", Env: []string{"K=V"}, Argv: []string{"sh", "-c", "echo hi"}, Cols: 80, Rows: 24},
		{Type: MPong, T: 1720000000},
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
		{"", "", true},         // auth off
		{"", "anything", true}, // auth off ignores whatever is presented
		{"secret", "secret", true},
		{"secret", "wrong", false},
		{"secret", "", false},
		{"secret", "secre", false},   // prefix must not pass
		{"secret", "secrett", false}, // superstring must not pass
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
		{[]int{1, 2}, []int{1, 2}, 2, true}, // highest common
		{[]int{1, 2}, []int{1}, 1, true},    // fall back to v1
		{[]int{2}, []int{1}, 0, false},      // no overlap -> loud failure
		{[]int{1}, []int{2}, 0, false},      // no overlap the other way
		{[]int{2, 1}, []int{1, 2}, 2, true}, // order-independent
		{nil, []int{1, 2}, 0, false},        // empty offer
	}
	for _, c := range cases {
		v, ok := Negotiate(c.offered, c.supported)
		if v != c.wantV || ok != c.wantOK {
			t.Errorf("Negotiate(%v,%v)=(%d,%v) want (%d,%v)", c.offered, c.supported, v, ok, c.wantV, c.wantOK)
		}
	}
}
