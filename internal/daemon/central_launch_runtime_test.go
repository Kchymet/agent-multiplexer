package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// TestCentralClaudeLaunchDeliversTokenInsideProtectedNamespace runs the real
// central launch path end to end: daemon-provisioned session authority, the
// signed mailbox served by the daemon's own session runtime, the protected
// namespace from panespec.Resolve, the payload trampoline's launcher, and a
// fake Claude. The token must reach only Claude's environment.
func TestCentralClaudeLaunchDeliversTokenInsideProtectedNamespace(t *testing.T) {
	if err := panespec.IsolationSupport(); err != nil {
		if os.Getenv("AMUX_REQUIRE_NAMESPACE_TEST") == "1" {
			t.Fatalf("required protected namespace unavailable: %v", err)
		}
		t.Skipf("protected namespace unavailable: %v", err)
	}
	t.Run("daemon-lifecycle", func(t *testing.T) { testCentralClaudeDaemonLifecycle(t, false) })
	t.Run("service-published-after-launch", func(t *testing.T) { testCentralClaudeDaemonLifecycle(t, true) })
	t.Run("subscription-delivery", func(t *testing.T) { testCentralClaudeTypedLaunch(t, "subscription") })
	t.Run("api-delivery", func(t *testing.T) { testCentralClaudeTypedLaunch(t, "api") })
}

func testCentralClaudeTypedLaunch(t *testing.T, mode string) {
	isolateHome(t)
	s := store.Session{ID: "central-owner", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "central-owner")}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, runtime, _ := sessionRuntimeFixture(t, s)
	t.Setenv("AMUX_JAIL", "on")
	ctx := t.Context()

	// A fake Claude under HOME: panespec mounts exactly this executable.
	observed := filepath.Join(s.Dir, "observed-claude")
	fake := filepath.Join(os.Getenv("HOME"), "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf 'token=%s\\napi=%s\\nargs=%s\\n' \"$CLAUDE_CODE_OAUTH_TOKEN\" \"${ANTHROPIC_API_KEY-unset}\" \"$*\" > " + observed + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", fake)

	tok, _ := centralauth.NewSecret(mockCentralToken)
	kind := centralauth.KindClaudeOAuthToken
	if mode == "api" {
		kind = centralauth.KindClaudeAPIKey
	}
	if _, err := centralauth.ClaudeStore().SaveKind(tok, kind, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Update(func(st *accounts.State) error {
		p := st.Profiles["claude:central"]
		p.Mode = mode
		st.Profiles[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	grant, err := d.authority.EnsureSession(ctx, s.ID, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := sessionrpc.OpenServerMailbox(s.ID, grant.MailboxHostDir, d.authority, d.authority,
		sessionrpc.Callbacks{Authorize: runtime.authorizeCallback, Dispatch: runtime.dispatchCallback}, sessionrpc.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if err := server.PublishService(ctx); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	defer stop()
	go func() { _ = server.Run(serving) }()

	_, env, argv, err := panespec.Resolve(panespec.LaunchSpec{Session: s, Access: grant}, panespec.TabAgent)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range append(append([]string(nil), argv...), env...) {
		if strings.Contains(field, mockCentralToken) {
			t.Fatalf("token in launch spec: %q", field)
		}
		if strings.Contains(field, core.AuthDir()) || strings.Contains(field, ".credentials.json") {
			t.Fatalf("central launch exposes a credential store: %q", field)
		}
	}
	// The launcher receives this pane's daemon-allocated process launch.
	if !regexp.MustCompile(regexp.QuoteMeta(centralauth.LauncherArg+" "+centralauth.LaunchArgPrefix) + `[0-9]+:[0-9a-f]{48} ` + regexp.QuoteMeta(fake)).MatchString(strings.Join(argv, " ")) {
		t.Fatalf("agent pane does not run the central launcher: %v", argv)
	}
	launchEnv, err := launchenv.Build(append(os.Environ(), "ANTHROPIC_API_KEY=inherited-must-not-reach-claude"), env, launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	run := func(timeout time.Duration) (string, error) {
		t.Helper()
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		cmd.Env = launchEnv
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), mockCentralToken) {
			t.Fatalf("token in pane output: %s", out)
		}
		return string(out), err
	}

	if out, err := run(30 * time.Second); err != nil {
		t.Fatalf("central launch: %v: %s", err, out)
	}
	b, err := os.ReadFile(observed)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	tokenValue, apiValue := mockCentralToken, "unset"
	if mode == "api" {
		tokenValue, apiValue = "", mockCentralToken
	}
	if !strings.Contains(got, "token="+tokenValue+"\n") || !strings.Contains(got, "api="+apiValue+"\n") {
		t.Fatalf("fake Claude observed:\n%s", got)
	}
	if !strings.Contains(got, "--permission-mode") {
		t.Fatalf("Claude argv not preserved through the launcher:\n%s", got)
	}

	// A revoked session never receives the credential: the launcher explains
	// and waits, and Claude never starts.
	if err := os.Remove(observed); err != nil {
		t.Fatal(err)
	}
	if err := d.authority.Revoke(ctx, access.SubjectSession, s.ID); err != nil {
		t.Fatal(err)
	}
	out, err := run(3 * time.Second)
	if err == nil || strings.Contains(out, "amux auth login") || !strings.Contains(out, "access was denied") {
		t.Fatalf("revoked launch = %v: %s", err, out)
	}
	if _, err := os.Stat(observed); !os.IsNotExist(err) {
		t.Fatalf("Claude started for a revoked session: %v", err)
	}
}

// Exercise production lifecycle gates and the reconciliation loop, unlike the
// direct mailbox server fixture above. PTY startup must return without waiting
// for the child's credential query: that query needs those same admission locks.
func testCentralClaudeDaemonLifecycle(t *testing.T, delayedService bool) {
	d, runtime, _ := sessionRuntimeFixture(t)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_SHUTDOWN_GRACE", "0s")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	s := store.Session{ID: "central-lifecycle", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "central-lifecycle")}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(os.Getenv("HOME"), "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(fake), 0700); err != nil {
		t.Fatal(err)
	}
	observed := filepath.Join(s.Dir, "launched")
	// Only a success marker is persisted; the synthetic credential stays in env.
	script := "#!/bin/sh\n[ \"$CLAUDE_CODE_OAUTH_TOKEN\" = " + mockCentralToken + " ] || exit 17\nprintf ready > " + observed + "\nsleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", fake)
	tok, _ := centralauth.NewSecret(mockCentralToken)
	if _, err := centralauth.ClaudeStore().Save(tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	d.engine = local.New()
	d.sessionRPC = runtime
	t.Cleanup(d.engine.Shutdown)
	startService := func() {
		t.Helper()
		if err := runtime.start(ctx); err != nil {
			t.Fatal(err)
		}
		serving, stop := context.WithCancel(ctx)
		joined := make(chan struct{})
		go func() { defer close(joined); runtime.run(serving) }()
		t.Cleanup(func() { stop(); <-joined; runtime.close() })
	}
	if !delayedService {
		startService()
	}
	// A real action holds dispatchMu/effectMu while starting the sandboxed PTY.
	finished := make(chan core.Result, 1)
	go func() { finished <- d.handle(ctx, core.Action{Action: core.ActionStart, ID: s.ID}) }()
	select {
	case result := <-finished:
		if !result.OK {
			t.Fatal(result.Error)
		}
	case <-ctx.Done():
		t.Fatal("start action blocked on its own credential query")
	}
	if delayedService {
		// Provisioning exists, but reconciliation has not published service.json.
		// Observe the actual child's failed query before allowing publication.
		instance, ok := d.engine.Lookup(engine.Key{AgentID: s.ID, Tab: panespec.TabAgent})
		if !ok {
			t.Fatal("missing launcher instance")
		}
		chunks := make(chan string, 32)
		unsubscribe := instance.Subscribe(engine.Sink{Output: func(b []byte) {
			select {
			case chunks <- string(b):
			default:
			}
		}})
		defer unsubscribe()
		output := ""
		for !strings.Contains(output, "retrying automatically") && !strings.Contains(output, "amux auth login") {
			select {
			case chunk := <-chunks:
				output += chunk
			case <-ctx.Done():
				t.Fatal("launcher did not report unpublished mailbox service")
			}
		}
		if strings.Contains(output, "amux auth login") || strings.Contains(output, mockCentralToken) || !strings.Contains(output, "mailbox request failed") {
			t.Fatalf("unpublished service misdiagnosed: %s", output)
		}
		startService()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if b, err := os.ReadFile(observed); err == nil && string(b) == "ready" {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("daemon reconciliation did not deliver the launch credential")
		}
	}
	t.Log("real daemon start action returned; reconciliation served the protected Claude credential request")
}
