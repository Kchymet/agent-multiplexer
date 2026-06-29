package muxproto

import (
	"net"
	"testing"

	"amux/internal/core"
)

func TestRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	ca, cb := NewConn(a), NewConn(b)
	defer ca.Close()
	defer cb.Close()

	// Client -> server, including raw control bytes (NUL/ESC) that must survive.
	go func() {
		_ = ca.WriteClient(ClientMsg{Type: CPaneInput, PaneID: "p1", Data: []byte{0x1b, 'O', 'K', 0x00}})
	}()
	m, err := cb.ReadClient()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != CPaneInput || m.PaneID != "p1" || string(m.Data) != "\x1bOK\x00" {
		t.Fatalf("client msg mismatch: %+v", m)
	}

	// Server -> client: a snapshot and a pane.output with a non-UTF8 byte.
	go func() {
		_ = cb.WriteServer(ServerMsg{Type: SSnapshot, Sessions: []core.Session{{ID: "x", Title: "t"}}})
		_ = cb.WriteServer(ServerMsg{Type: SPaneOutput, PaneID: "p1", Data: []byte("hi\xff")})
	}()
	s1, err := ca.ReadServer()
	if err != nil {
		t.Fatal(err)
	}
	if s1.Type != SSnapshot || len(s1.Sessions) != 1 || s1.Sessions[0].ID != "x" {
		t.Fatalf("snapshot mismatch: %+v", s1)
	}
	s2, err := ca.ReadServer()
	if err != nil {
		t.Fatal(err)
	}
	if s2.Type != SPaneOutput || string(s2.Data) != "hi\xff" {
		t.Fatalf("output mismatch: %+v", s2)
	}
}
