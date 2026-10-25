//go:build linux

package codexapp

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/codexsocket"
	"amux/internal/core"
)

func TestDialWSOverCodex157ProtectedUnixAlias(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	rendezvous := filepath.Join(core.DataDir(), "cx", "0123456789abcdef", "cx.sock")
	if err := os.MkdirAll(filepath.Dir(rendezvous), 0o700); err != nil {
		t.Fatal(err)
	}
	source, _, err := codexsocket.PrepareNamespaceMount(rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	target, err := codexsocket.ExpectedNamespacePath(rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(source, filepath.Base(target))
	listener, err := net.Listen("unix", physical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(physical, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: echoHandler()}
	go server.Serve(listener)
	defer server.Close()
	if err := os.Symlink(target, rendezvous); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialWS(ctx, "unix://"+rendezvous, "")
	if err != nil {
		t.Fatalf("dial protected Codex 0.157 alias: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(ctx, []byte("codex-0.157")); err != nil {
		t.Fatal(err)
	}
	got, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "codex-0.157" {
		t.Fatalf("echo = %q", got)
	}
}
