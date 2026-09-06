package panespec

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/store"
)

// A delayed second caller can construct its launch after the first caller has
// started the App Server, but before Manager.Ensure returns the cached instance.
// Command construction must leave that live listener available to new clients.
func TestAppServerCommandPreservesLiveSocket(t *testing.T) {
	home, err := os.MkdirTemp("", "cx-life-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CODEX_BIN", "/bin/true")
	dir := filepath.Join(home, "work")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "socket-owner", Agent: "codex", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, _, endpoint, err := AppServerCommand("socket-owner")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimPrefix(endpoint, "unix://")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	assertConnectable := func() {
		t.Helper()
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err != nil {
			t.Fatalf("live App Server endpoint was lost: %v", err)
		}
		conn.Close()
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		accepted.Close()
	}
	assertConnectable()
	_, _, _, again, err := AppServerCommand("socket-owner")
	if err != nil {
		t.Fatal(err)
	}
	if again != endpoint {
		t.Fatalf("endpoint changed: %q -> %q", endpoint, again)
	}
	assertConnectable()
}
