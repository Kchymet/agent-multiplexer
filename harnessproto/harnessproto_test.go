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

// TestHarnessCapabilityModelsAdditive pins the per-harness model list as an
// additive, omitempty field: a provider that advertises models round-trips them
// in order, and one that does not (an older provider) emits no "models" key, so
// a consumer must treat absence as "not advertised".
func TestHarnessCapabilityModelsAdditive(t *testing.T) {
	in := HarnessCapability{Name: "claude", Version: "2.1.0", Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"name":"claude","version":"2.1.0","models":["claude-opus-5-5","claude-sonnet-5-5"]}`; string(b) != want {
		t.Fatalf("wire = %s, want %s", b, want)
	}
	var out HarnessCapability
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 2 || out.Models[0] != "claude-opus-5-5" || out.Models[1] != "claude-sonnet-5-5" {
		t.Fatalf("decoded models = %v", out.Models)
	}
	legacy, err := json.Marshal(HarnessCapability{Name: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if string(legacy) != `{"name":"codex"}` {
		t.Fatalf("legacy wire = %s, want no models key", legacy)
	}
}

// TestComputeCapabilityIsExplicit pins the fail-closed compatibility rule:
// older providers omit the additive field and therefore do not grant compute.
// Only a literal true value authorizes placement and pane control.
func TestComputeCapabilityIsExplicit(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy absent":  `{}`,
		"explicit false": `{"compute":false}`,
		"explicit true":  `{"compute":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got Capabilities
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if got.Compute != (name == "explicit true") {
				t.Fatalf("Compute = %v for %s", got.Compute, raw)
			}
		})
	}
}
