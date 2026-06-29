package harnessproto

import (
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
