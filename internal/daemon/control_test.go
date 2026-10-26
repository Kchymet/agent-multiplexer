package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/store"
)

func isolateControl(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_RUNTIME_DIR", home)
	t.Setenv("AMUX_SOCK", filepath.Join(home, "daemon.sock"))
	t.Setenv(amuxcfg.ControlEnv, "")
	if err := os.Unsetenv(amuxcfg.ControlEnv); err != nil {
		t.Fatal(err)
	}
}

// legacyClaudeAuth selects the host Claude login for tests of surfaces that
// predate central auth; central delivery has its own tests.
func legacyClaudeAuth(t *testing.T) {
	t.Helper()
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthHost); err != nil {
		t.Fatal(err)
	}
}

// legacyCodexAuth selects the host Codex login, under which codex.control
// keeps its own selection; central Codex auth always requires the App Server.

func TestPersistedControlSurvivesFreshDaemon(t *testing.T) {
	isolateControl(t)
	if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		d := New("", nil, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		var c *Client
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var err error
			c, err = Dial()
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if c == nil {
			cancel()
			t.Fatalf("daemon did not start: %v", <-done)
		}
		selection, err := c.CodexControl()
		_ = c.Close()
		if err != nil || selection.Effective != amuxcfg.AppServer || selection.Source != "config" || selection.OverrideSet {
			cancel()
			<-done
			t.Fatalf("startup %d: %+v, %v", i, selection, err)
		}
		if !d.structuredControl(store.Session{Agent: "codex"}) {
			cancel()
			<-done
			t.Fatal("running daemon did not enable App Server routing")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("private daemon did not stop")
		}
	}
}

func TestControlFrozenForDaemonLifetime(t *testing.T) {
	isolateControl(t)
	if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.codex = codexapp.NewManager(ctx, "")
	defer d.codex.Shutdown()
	c, closeClient := dialDaemon(t, d)
	defer closeClient()
	if err := amuxcfg.SetCodexControl(amuxcfg.PTY); err != nil {
		t.Fatal(err)
	}
	t.Setenv(amuxcfg.ControlEnv, amuxcfg.PTY)
	// Routing and diagnostics keep the startup snapshot. An ordinary Codex
	// member shows routing; a goal coordinator is always structured.
	member := store.Session{ID: "codex", RootID: "root", Agent: "codex"}
	if !d.structuredControl(member) || !d.structuredResolvable(member) {
		t.Fatal("config/environment edit changed the existing daemon's routing")
	}
	selection, err := c.CodexControl()
	if err != nil || selection.Effective != amuxcfg.AppServer || selection.Persisted != amuxcfg.AppServer || selection.OverrideSet {
		t.Fatalf("daemon diagnostics re-read startup inputs: %+v, %v", selection, err)
	}
	// A fresh daemon reads the edit, but central Codex auth keeps App Server
	// control and reports the ignored PTY selection.
	fresh := New("", nil, time.Hour)
	if fresh.codexControl.Effective != amuxcfg.AppServer || fresh.codexControl.Persisted != amuxcfg.PTY || fresh.codexControl.Warning == "" {
		t.Fatalf("fresh daemon control = %+v", fresh.codexControl)
	}
}

func TestControlQueryReportsImmutableStartupSnapshot(t *testing.T) {
	for _, override := range []string{"absent", "", "pty", "app-server", "typo"} {
		t.Run(override, func(t *testing.T) {
			isolateControl(t)
			if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
				t.Fatal(err)
			}
			if override != "absent" {
				t.Setenv(amuxcfg.ControlEnv, override)
			}
			d := New("", nil, time.Hour)
			want := d.codexControl
			c, closeClient := dialDaemon(t, d)
			defer closeClient()
			before, err := c.CodexControl()
			if err != nil || before != want {
				t.Fatalf("initial query = %+v, %v; want %+v", before, err, want)
			}
			// A subsequent malformed config must not break diagnostics for the
			// healthy daemon, or change its captured path, source, or override.
			if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":false}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(amuxcfg.ControlEnv, "different-invalid-override")
			after, err := c.CodexControl()
			if err != nil || after != want {
				t.Fatalf("query reloaded changed inputs: %+v, %v; want %+v", after, err, want)
			}
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			after, err = c.CodexControl()
			if err != nil || after != want {
				t.Fatalf("query changed startup config path: %+v, %v; want %+v", after, err, want)
			}
		})
	}
}

func TestInvalidControlRejectsBeforeSocketMutation(t *testing.T) {
	isolateControl(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":"broken"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.SocketPath(), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	if err := d.Run(context.Background()); err == nil {
		t.Fatal("invalid config started a daemon")
	}
	b, err := os.ReadFile(core.SocketPath())
	if err != nil || string(b) != "untouched" {
		t.Fatalf("startup touched the socket: %q, %v", b, err)
	}
}
