package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	t.Run("daemon-lifecycle", func(t *testing.T) { testCentralClaudeDaemonLifecycle(t, lifecyclePreOpened) })
	t.Run("service-published-after-launch", func(t *testing.T) { testCentralClaudeDaemonLifecycle(t, lifecycleDelayedService) })
	t.Run("created-while-serving", func(t *testing.T) { testCentralClaudeDaemonLifecycle(t, lifecycleCreatedWhileServing) })
	t.Run("fresh-launch-under-releasing-client-pressure", testCentralClaudeUnderClientPressure)
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
	if !strings.Contains(strings.Join(argv, " "), centralauth.LauncherArg+" "+fake) {
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

type lifecycleMode int

const (
	// The session exists before the runtime starts, so its mailbox is open.
	lifecyclePreOpened lifecycleMode = iota
	// The runtime publishes service only after the launcher is already waiting.
	lifecycleDelayedService
	// The session is stored and started while the run loop is serving: its
	// mailbox is published by a reconcile that must wait for the start action.
	lifecycleCreatedWhileServing
)

// Exercise production lifecycle gates and the reconciliation loop, unlike the
// direct mailbox server fixture above. PTY startup must return without waiting
// for the child's credential query: that query needs those same admission locks.
// In every mode the launcher's first credential attempt must succeed: a miss
// prints retry guidance and costs at least one LaunchRetry.
func testCentralClaudeDaemonLifecycle(t *testing.T, mode lifecycleMode) {
	d, runtime, _ := sessionRuntimeFixture(t)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_SHUTDOWN_GRACE", "0s")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	s := store.Session{ID: "central-lifecycle", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "central-lifecycle")}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	putSession := func() {
		t.Helper()
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
	}
	if mode != lifecycleCreatedWhileServing {
		putSession()
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
	if mode != lifecycleDelayedService {
		startService()
	}
	if mode == lifecycleCreatedWhileServing {
		putSession()
	}
	// A real action holds dispatchMu/effectMu while starting the sandboxed PTY.
	began := time.Now()
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
	instance, ok := d.engine.Lookup(engine.Key{AgentID: s.ID, Tab: panespec.TabAgent})
	if !ok {
		t.Fatal("missing launcher instance")
	}
	var outputMu sync.Mutex
	output := ""
	unsubscribe := instance.Subscribe(engine.Sink{Output: func(b []byte) {
		outputMu.Lock()
		output += string(b)
		outputMu.Unlock()
	}})
	defer unsubscribe()
	if mode == lifecycleDelayedService {
		// Provisioning exists, but reconciliation has not published service.json.
		// The launcher must keep its first attempt open rather than fail it.
		time.Sleep(time.Second)
		if _, err := os.Stat(observed); !os.IsNotExist(err) {
			t.Fatalf("Claude started before the mailbox was published: %v", err)
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
	elapsed := time.Since(began)
	outputMu.Lock()
	defer outputMu.Unlock()
	if strings.Contains(output, "retrying automatically") || strings.Contains(output, mockCentralToken) {
		t.Fatalf("first credential attempt failed: %q", output)
	}
	if elapsed >= centralauth.LaunchRetry {
		t.Fatalf("credential delivered after %v, not within the first attempt", elapsed)
	}
	t.Logf("first credential attempt delivered %v after the real start action", elapsed)
}

// testCentralClaudeUnderClientPressure runs real production clients against the
// real session runtime and its daemon-global response budget. Six sessions'
// fake Claude repeatedly re-invokes the protected central launcher inside its
// own namespace; each invocation is one signed credential fetch and release.
// That is far more traffic than the pool sustains under age-only retirement
// (128 responses per 60s), so a fresh session's launcher must still be served
// on its first attempt, every background fetch must succeed, and the pool must
// stay bounded by in-flight work.
func testCentralClaudeUnderClientPressure(t *testing.T) {
	const background, fetches = 6, 40
	var sessions []store.Session
	for i := 0; i < background; i++ {
		sessions = append(sessions, store.Session{ID: fmt.Sprintf("pressure-%d", i), RootID: "root", Agent: "claude"})
	}
	d, runtime, _ := sessionRuntimeFixture(t, sessions...)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_SHUTDOWN_GRACE", "0s")
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	fake := filepath.Join(os.Getenv("HOME"), "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(fake), 0700); err != nil {
		t.Fatal(err)
	}
	// Background sessions loop the real launcher through their pane's own
	// payload trampoline, exec'ing true once delivered; the fresh session only
	// marks delivery. Linux mounts the trampoline at a fixed tool path; macOS
	// publishes a per-session copy.
	trampoline := "case \"$PWD\" in\n"
	for _, session := range sessions {
		tool := launchenv.ToolBinDir + "/amux"
		if goruntime.GOOS == "darwin" {
			tool = core.SessionBinPath(session.ID)
		}
		trampoline += "  */" + session.ID + ") tool='" + tool + "';;\n"
	}
	trampoline += "esac\n"
	script := "#!/bin/sh\n[ \"$CLAUDE_CODE_OAUTH_TOKEN\" = " + mockCentralToken + " ] || exit 17\n" +
		"case \"$PWD\" in */fresh) printf ready > \"$PWD/launched\"; sleep 60; exit 0;; esac\n" + trampoline +
		"i=0\nwhile [ $i -lt " + fmt.Sprint(fetches) + " ]; do\n" +
		"  AMUX_PAYLOAD_CLEAN_EXEC=1 \"$tool\" --amux-payload-clean-exec " + centralauth.LauncherArg + " true || exit 1\n" +
		"  i=$((i+1)); printf %s $i > \"$PWD/fetched\"\ndone\nsleep 60\n"
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
	if err := runtime.start(ctx); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	joined := make(chan struct{})
	go func() { defer close(joined); runtime.run(serving) }()
	t.Cleanup(func() { stop(); <-joined; runtime.close() })

	budget := runtime.responseBudget.(*sessionResponseBudget)
	var peak atomic.Int64
	sampling, stopSampling := context.WithCancel(ctx)
	defer stopSampling()
	go func() {
		for sampling.Err() == nil {
			budget.mu.Lock()
			files := int64(budget.files)
			budget.mu.Unlock()
			if files > peak.Load() {
				peak.Store(files)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	fetched := func(id string) int {
		b, _ := os.ReadFile(filepath.Join(core.SessionsDir(), "root", id, "fetched"))
		n, _ := strconv.Atoi(string(b))
		return n
	}
	for _, session := range sessions {
		if r := d.handle(ctx, core.Action{Action: core.ActionStart, ID: session.ID}); !r.OK {
			t.Fatal(r.Error)
		}
	}
	// Let background traffic pass the age-only capacity before the fresh launch.
	for total := 0; total < sessionrpc.MaxResponseFiles+32; {
		total = 0
		for _, session := range sessions {
			total += fetched(session.ID)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("background fetches stalled at %d", total)
		case <-time.After(20 * time.Millisecond):
		}
	}

	fresh := store.Session{ID: "fresh", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "fresh")}
	if err := os.MkdirAll(fresh.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(fresh); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if r := d.handle(ctx, core.Action{Action: core.ActionStart, ID: fresh.ID}); !r.OK {
		t.Fatal(r.Error)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(fresh.Dir, "launched")); err == nil && string(b) == "ready" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fresh launcher was not served under client pressure")
		case <-time.After(10 * time.Millisecond):
		}
	}
	elapsed := time.Since(began)
	for _, session := range sessions {
		for fetched(session.ID) < fetches {
			select {
			case <-ctx.Done():
				t.Fatalf("%s completed %d/%d credential fetches", session.ID, fetched(session.ID), fetches)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	stopSampling()
	for _, id := range append([]string{fresh.ID}, func() (ids []string) {
		for _, s := range sessions {
			ids = append(ids, s.ID)
		}
		return
	}()...) {
		instance, ok := d.engine.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent})
		if !ok {
			t.Fatalf("missing runtime for %s", id)
		}
		var output strings.Builder
		instance.Subscribe(engine.Sink{Output: func(b []byte) { output.Write(b) }})()
		if strings.Contains(output.String(), "retrying automatically") || strings.Contains(output.String(), mockCentralToken) {
			t.Fatalf("%s launcher output: %q", id, output.String())
		}
	}
	if elapsed >= centralauth.LaunchRetry {
		t.Fatalf("fresh credential delivered after %v under pressure", elapsed)
	}
	if p := peak.Load(); p > sessionrpc.MaxResponseFiles/4 {
		t.Fatalf("response pool peaked at %d files; consumed responses are not being retired", p)
	}
	t.Logf("%d releasing fetches; fresh first attempt %v; pool peak %d files", background*fetches, elapsed, peak.Load())
}
