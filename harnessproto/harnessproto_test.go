package harnessproto

import (
	"encoding/json"
	"net"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	mux, h := NewConn(a), NewConn(b)
	defer mux.Close()
	defer h.Close()

	// Mux -> harness: spawn, then input with control bytes.
	go func() {
		_ = mux.WriteMux(MuxMsg{Type: MSpawn, PaneID: "p1", Dir: "/tmp", Argv: []string{"sh"}, Cols: 80, Rows: 24})
		_ = mux.WriteMux(MuxMsg{Type: MInput, PaneID: "p1", Data: []byte{0x03}}) // Ctrl-C
	}()
	s, err := h.ReadMux()
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != MSpawn || s.PaneID != "p1" || len(s.Argv) != 1 || s.Argv[0] != "sh" {
		t.Fatalf("spawn mismatch: %+v", s)
	}
	in, err := h.ReadMux()
	if err != nil {
		t.Fatal(err)
	}
	if in.Type != MInput || len(in.Data) != 1 || in.Data[0] != 0x03 {
		t.Fatalf("input mismatch: %+v", in)
	}

	// Harness -> mux: output (non-UTF8) then exit.
	go func() {
		_ = h.WriteHarness(HarnessMsg{Type: HOutput, PaneID: "p1", Data: []byte("x\xff")})
		_ = h.WriteHarness(HarnessMsg{Type: HExit, PaneID: "p1", Error: "boom"})
	}()
	o, err := mux.ReadHarness()
	if err != nil {
		t.Fatal(err)
	}
	if o.Type != HOutput || string(o.Data) != "x\xff" {
		t.Fatalf("output mismatch: %+v", o)
	}
	e, err := mux.ReadHarness()
	if err != nil {
		t.Fatal(err)
	}
	if e.Type != HExit || e.Error != "boom" {
		t.Fatalf("exit mismatch: %+v", e)
	}
}

// TestExecutionCapabilitiesEmptyVsAbsent preserves the distinction needed for
// rollout: no execution block is a legacy provider, while a present empty block
// says this provider ran discovery and verified no usable harness.
func TestExecutionCapabilitiesEmptyVsAbsent(t *testing.T) {
	cases := []struct {
		name    string
		in      Capabilities
		present bool
	}{
		{"legacy absent", Capabilities{}, false},
		{"verified none", Capabilities{Execution: &ExecutionCapabilities{
			Harnesses: []HarnessCapability{}, IdentityModes: []string{IdentityMachine},
		}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			var out Capabilities
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatal(err)
			}
			if (out.Execution != nil) != tc.present {
				t.Fatalf("wire %s decoded execution %+v, present=%v", b, out.Execution, tc.present)
			}
			if tc.present && (len(out.Execution.Harnesses) != 0 || out.Execution.Supports("claude", IdentityMachine)) {
				t.Fatalf("empty verified execution became usable: %+v", out.Execution)
			}
		})
	}
}
