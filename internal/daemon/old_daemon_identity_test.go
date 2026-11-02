package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/hostbackend"
)

// TestOldDaemonIdentityCompat drives this build's production client path —
// hostbackend over the real authenticated unix socket, and the direct server
// through direct.HostBackend — against a daemon that predates creation
// identities, in an isolated HOME/XDG/socket. It proves that a creation is
// refused before anything that could create is sent (the old daemon's store
// gains no session), that capabilities report the upgrade, and that ordinary
// reads keep working. No creation succeeds, so no agent runtime starts.
//
// The old daemon is one of:
//   - AMUX_OLD_DAEMON_BIN: an older amux binary run as `amux daemon` (a real
//     daemon process; it refuses to start inside an amux agent sandbox);
//   - AMUX_OLD_DAEMON_SRC: an older source tree (e.g. `git archive c97c8d6`),
//     whose own daemon code serves the real host wire and handshake over a
//     unix socket through its test harness (testdata/oldserve_test.go.txt);
//     only its agent runtime engine is fake.
func TestOldDaemonIdentityCompat(t *testing.T) {
	bin, src := os.Getenv("AMUX_OLD_DAEMON_BIN"), os.Getenv("AMUX_OLD_DAEMON_SRC")
	if bin == "" && src == "" {
		t.Skip("set AMUX_OLD_DAEMON_BIN or AMUX_OLD_DAEMON_SRC to an amux that predates creation identities")
	}
	// The old tree's `go test` keeps this build's Go caches, not the
	// isolated HOME's.
	goEnv := []string{}
	if src != "" {
		out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE", "GOPATH").Output()
		if err != nil {
			t.Fatal(err)
		}
		vals := strings.Split(strings.TrimSpace(string(out)), "\n")
		goEnv = []string{"GOMODCACHE=" + vals[0], "GOCACHE=" + vals[1], "GOPATH=" + vals[2], "GOFLAGS=-mod=mod"}
	}
	home := t.TempDir()
	env := map[string]string{
		"HOME":              home,
		"XDG_DATA_HOME":     filepath.Join(home, "data"),
		"XDG_CONFIG_HOME":   filepath.Join(home, "config"),
		"XDG_STATE_HOME":    filepath.Join(home, "state"),
		"XDG_RUNTIME_DIR":   filepath.Join(home, "run"),
		"AMUX_SOCK":         filepath.Join(home, "run", "amux.sock"),
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"CODEX_HOME":        filepath.Join(home, ".codex"),
	}
	if err := os.MkdirAll(env["XDG_RUNTIME_DIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	logPath := filepath.Join(home, "daemon.log")
	logf, _ := os.Create(logPath)
	var cmd *exec.Cmd
	if bin != "" {
		cmd = exec.Command(bin, "daemon")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	} else {
		helper, err := os.ReadFile(filepath.Join("testdata", "oldserve_test.go.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "internal", "daemon", "zz_oldserve_test.go"), helper, 0o644); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command("go", "test", "./internal/daemon", "-run", "^TestServeOldForCrossVersion$", "-count=1", "-v")
		cmd.Dir = src
		cmd.Env = append(append(os.Environ(), goEnv...), "OLD_SERVE_SOCK="+env["AMUX_SOCK"],
			"OLD_SERVE_CRED="+access.DefaultCredentialDir(access.SubjectHost, access.LocalHostSubject))
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(env["AMUX_SOCK"]+".stop", []byte("1"), 0o600)
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			t.Logf("old daemon log:\n%s", b)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if src != "" {
		for {
			if _, err := os.Stat(env["AMUX_SOCK"] + ".ready"); err == nil {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("old daemon harness never became ready")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	var c *daemon.Client
	for {
		var err error
		if c, err = daemon.DialContext(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("old daemon never accepted a host connection: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer c.Close()
	version, err := c.Version()
	if err != nil {
		t.Fatalf("ordinary query on the old daemon: %v", err)
	}
	t.Logf("old daemon: version %s protocol %d schema %d", version.DaemonVersion, version.DaemonProtocol, version.DatabaseSchema)
	roots := func() int {
		t.Helper()
		raw, err := c.Query(core.QuerySessions)
		if err != nil {
			t.Fatalf("sessions query: %v", err)
		}
		var rows []core.WorkgroupRow
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Role == "coordinator" {
				n++
			}
		}
		return n
	}
	before := roots()

	// The connection-level protocol: negotiation fails, and a creation on an
	// un-negotiated connection is refused without being sent.
	if err := c.NegotiateCreationIdentity(); !errors.Is(err, daemon.ErrDaemonUpgradeRequired) {
		t.Fatalf("negotiation with the old daemon = %v", err)
	}
	empty := core.Action{Action: core.ActionCreateWorkspace, Fields: map[string]string{"name": "must-not-exist", "repos": ""}}
	if _, err := c.DispatchIdentity(empty, daemon.IdentityRequest{}); !errors.Is(err, daemon.ErrDaemonUpgradeRequired) {
		t.Fatalf("identity dispatch to the old daemon = %v", err)
	}

	// The production backend: a fresh connection per call, negotiated first.
	h := hostbackend.New()
	if err := h.CreationIdentity(ctx); !errors.Is(err, hostbackend.ErrUpgradeRequired) {
		t.Fatalf("CreationIdentity = %v", err)
	}
	if _, err := h.ApplyIdentity(ctx, empty, ""); !errors.Is(err, hostbackend.ErrUpgradeRequired) {
		t.Fatalf("ApplyIdentity = %v", err)
	}
	if _, err := h.SessionIdentities(ctx, []string{"x"}); !errors.Is(err, hostbackend.ErrUpgradeRequired) {
		t.Fatalf("SessionIdentities = %v", err)
	}

	// The direct server over the same old daemon.
	auth, err := direct.OpenAuthority(t.TempDir(), direct.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	backend := direct.HostBackend{H: h, Probe: func(context.Context) []direct.CatalogHarness {
		return []direct.CatalogHarness{{Name: "claude"}, {Name: "codex"}}
	}}
	srv, err := direct.NewServer(direct.Config{Authority: auth, Backend: backend, Endpoints: []string{"https://" + e2eHost}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := auth.NewCode("old", everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions}, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.RedeemCode(code, "phone", direct.ClientAndroid)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any) (int, string) {
		var rd *strings.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		} else {
			rd = strings.NewReader("")
		}
		r := httptest.NewRequest(method, path, rd)
		r.Host = e2eHost
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if code, b := call("GET", direct.PathCapabilities, nil); code != http.StatusOK || !strings.Contains(b, `"daemon_upgrade_required":true`) || !strings.Contains(b, `"create_workgroup":false`) {
		t.Fatalf("capabilities: %d %s", code, b)
	}
	for _, async := range []bool{false, true} {
		code, b := call("POST", direct.PathWorkgroups, direct.ActionRequest{Verb: direct.VerbNewWorkgroupEmpty,
			Fields: map[string]string{"name": "must-not-exist", "repos": ""}, IdemKey: "k", Async: async})
		if code != http.StatusServiceUnavailable || !strings.Contains(b, direct.CodeDaemonUpgradeRequired) {
			t.Fatalf("direct create (async=%v): %d %s", async, code, b)
		}
	}
	if code, b := call("GET", direct.PathSession, nil); code != http.StatusOK {
		t.Fatalf("ordinary inventory read: %d %s", code, b)
	}
	if after := roots(); after != before {
		t.Fatalf("the old daemon gained %d workgroups", after-before)
	}

	// Reconnect across versions: one backend and one direct server whose
	// connections go to a current daemon, then the old one (a downgrade),
	// then the current one again (an upgrade). Each fresh connection
	// negotiates, so creation follows the daemon actually reached.
	current := daemon.NewLiveHostForTest(t)
	for k, v := range env { // NewLiveHostForTest isolated HOME; restore ours
		t.Setenv(k, v)
	}
	var useOld atomic.Bool
	switching := hostbackend.WithDialer(func(ctx context.Context) (hostbackend.Conn, error) {
		if useOld.Load() {
			return daemon.DialContext(ctx)
		}
		return current.Dial(ctx)
	})
	sw, err := direct.NewServer(direct.Config{Authority: auth, Backend: direct.HostBackend{H: switching, Dial: current.Dial,
		Probe: func(context.Context) []direct.CatalogHarness {
			return []direct.CatalogHarness{{Name: "claude"}, {Name: "codex"}}
		}},
		Endpoints: []string{"https://" + e2eHost}})
	if err != nil {
		t.Fatal(err)
	}
	srv = sw
	ws := core.Action{Action: core.ActionCreateWorkspace, Fields: map[string]string{"name": "reconnect", "repos": ""}}
	for i, old := range []bool{false, true, false} {
		useOld.Store(old)
		err := switching.CreationIdentity(ctx)
		_, b := call("GET", direct.PathCapabilities, nil)
		upgrade := strings.Contains(b, `"daemon_upgrade_required":true`)
		if old {
			if !errors.Is(err, hostbackend.ErrUpgradeRequired) || !upgrade {
				t.Fatalf("step %d (old daemon): negotiation %v, capabilities %s", i, err, b)
			}
			if _, err := switching.ApplyIdentity(ctx, ws, ""); !errors.Is(err, hostbackend.ErrUpgradeRequired) {
				t.Fatalf("step %d: create on the old daemon = %v", i, err)
			}
			continue
		}
		if err != nil || upgrade {
			t.Fatalf("step %d (current daemon): negotiation %v, capabilities %s", i, err, b)
		}
		ident, err := switching.ApplyIdentity(ctx, ws, "")
		if err != nil || !ident.Complete() {
			t.Fatalf("step %d: create on the current daemon = %+v %v", i, ident, err)
		}
	}
	if after := roots(); after != before {
		t.Fatalf("the old daemon gained %d workgroups during the reconnects", after-before)
	}
}
