package codexapp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// echoHandler is a WebSocket server that echoes each message back — enough to
// prove dialWS completes a real RFC6455 handshake and round-trips messages over
// either a unix socket or loopback TCP.
func echoHandler() http.Handler {
	return websocket.Handler(func(ws *websocket.Conn) {
		for {
			var b []byte
			if err := websocket.Message.Receive(ws, &b); err != nil {
				return
			}
			if err := websocket.Message.Send(ws, string(b)); err != nil {
				return
			}
		}
	})
}

func TestDialWSOverUnix(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "ws.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	srv := &http.Server{Handler: echoHandler()}
	go srv.Serve(ln)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWS(ctx, "unix://"+sock)
	if err != nil {
		t.Fatalf("dialWS unix: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage([]byte(`{"hello":1}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != `{"hello":1}` {
		t.Fatalf("echo = %q", got)
	}
}

func TestDialWSOverLoopbackTCP(t *testing.T) {
	ts := httptest.NewServer(echoHandler())
	defer ts.Close()
	endpoint := "ws://" + strings.TrimPrefix(ts.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWS(ctx, endpoint)
	if err != nil {
		t.Fatalf("dialWS loopback tcp: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("echo = %q", got)
	}
}

func TestDialWSRefusesNonLoopback(t *testing.T) {
	// A non-loopback ws:// (no TLS, no auth) must be refused before any dial.
	_, err := dialWS(context.Background(), "ws://10.0.0.1:4500")
	if err == nil {
		t.Fatal("expected refusal of a non-loopback ws:// endpoint")
	}
	if !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("err = %v, want a non-loopback refusal", err)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for _, h := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		if !isLoopbackHost(h) {
			t.Errorf("%q should be loopback", h)
		}
	}
	for _, h := range []string{"10.0.0.1", "example.com", "0.0.0.0", "8.8.8.8"} {
		if isLoopbackHost(h) {
			t.Errorf("%q should not be loopback", h)
		}
	}
}
