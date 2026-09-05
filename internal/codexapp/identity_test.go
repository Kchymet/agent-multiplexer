package codexapp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestIdentitySaveLoadRemove(t *testing.T) {
	// Redirect state + runtime dirs into the test's temp so nothing touches the
	// real amux state.
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp) // not read by core, so also pin HOME
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)

	id := Identity{
		SessionID:   "sess-1",
		SocketPath:  SocketPathFor("sess-1"),
		ThreadID:    "thr-9",
		ControlMode: harnessproto.ControlModeStructured,
	}
	if err := SaveIdentity(id); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok := LoadIdentity("sess-1")
	if !ok {
		t.Fatal("load: not found after save")
	}
	if got != id {
		t.Fatalf("loaded %+v, want %+v", got, id)
	}
	if err := RemoveIdentity("sess-1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := LoadIdentity("sess-1"); ok {
		t.Fatal("identity still present after remove")
	}
	// Removing a missing identity is not an error.
	if err := RemoveIdentity("sess-1"); err != nil {
		t.Fatalf("remove missing: %v", err)
	}
}

func TestLoadMissingIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, ok := LoadIdentity("nope"); ok {
		t.Fatal("expected not-found for a session never saved")
	}
}

func TestSocketPathShapeAndSanitize(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	p := SocketPathFor("a/b/../c!") // path-hostile id
	if strings.ContainsAny(filepath.Base(p), "/!") {
		t.Fatalf("socket basename not sanitized: %q", p)
	}
	if !strings.HasSuffix(p, ".sock") {
		t.Fatalf("socket path missing .sock suffix: %q", p)
	}
	if filepath.Dir(p) != "/run/user/1000/codexapp" {
		t.Fatalf("socket dir = %q", filepath.Dir(p))
	}
}

func TestSanitizeEmptyFallsBack(t *testing.T) {
	if got := sanitize("///"); got != "session" {
		t.Fatalf("sanitize empty = %q, want session", got)
	}
}

func TestAttachArgvShape(t *testing.T) {
	argv := AttachArgv("codex", "/run/user/1000/codexapp/s.sock", "thr-1")
	want := []string{"codex", "--remote", "unix:///run/user/1000/codexapp/s.sock", "resume", "thr-1"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Fatalf("attach argv = %v, want %v", argv, want)
	}
	// No thread id → no resume subcommand.
	argv = AttachArgv("", "/s.sock", "")
	if strings.Join(argv, " ") != "codex --remote unix:///s.sock" {
		t.Fatalf("attach argv (no thread) = %v", argv)
	}
}

func TestAppServerArgvShape(t *testing.T) {
	argv := AppServerArgv("codex", "/s.sock")
	if strings.Join(argv, " ") != "codex app-server --listen unix:///s.sock" {
		t.Fatalf("app-server argv = %v", argv)
	}
}
