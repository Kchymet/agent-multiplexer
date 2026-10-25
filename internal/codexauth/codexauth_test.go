package codexauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newFake(t *testing.T, env ...string) (*Provider, string, *syncBuffer) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "auth", "codex")
	stderr := &syncBuffer{}
	base := append([]string{
		fakeEnv + "=1", "PATH=" + os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-should-be-scrubbed-0000", "CODEX_API_KEY=x", "RUST_LOG=trace",
		"CODEX_HOME=/elsewhere",
	}, env...)
	p, err := New(Config{Argv: []string{os.Args[0]}, Home: home, Env: base, Stderr: stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, home, stderr
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func login(t *testing.T, p *Provider) {
	t.Helper()
	var shown []string
	if err := p.Login(ctxT(t), func(u string) { shown = append(shown, u) }); err != nil {
		t.Fatal(err)
	}
	if len(shown) != 1 || shown[0] != "https://auth.example/authorize?state=s" {
		t.Fatalf("auth URL shown %q", shown)
	}
}

func assertNoSecret(t *testing.T, what string, s string) {
	t.Helper()
	if strings.Contains(s, refreshSecret) || strings.Contains(s, "eyJ") {
		t.Fatalf("%s leaks a credential: %s", what, s)
	}
}

func TestLoggedOut(t *testing.T) {
	p, _, _ := newFake(t)
	if _, err := p.Tokens(ctxT(t)); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Tokens = %v, want ErrNotLoggedIn", err)
	}
	if a, err := p.Status(ctxT(t)); err != nil || a.Type != "" {
		t.Fatalf("Status = %+v, %v", a, err)
	}
}

func TestLoginTokensAndRedaction(t *testing.T) {
	p, home, stderr := newFake(t)
	login(t, p)
	tok, err := p.SessionToken(ctxT(t), "s1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.AccountID != "acct-1" || tok.PlanType != "plus" || tok.Generation != 1 {
		t.Fatalf("tokens %v", tok)
	}
	if time.Until(tok.ExpiresAt) < 50*time.Minute {
		t.Fatalf("exp %v", tok.ExpiresAt)
	}
	js, _ := json.Marshal(tok)
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%+v", tok), fmt.Sprintf("%#v", tok), string(js)} {
		if strings.Contains(s, tok.AccessToken) {
			t.Fatalf("formatting leaks the access token: %s", s)
		}
		assertNoSecret(t, "formatted tokens", s)
	}
	// No field of the projection can hold the refresh token.
	v := reflect.ValueOf(tok)
	for i := range v.NumField() {
		if strings.Contains(fmt.Sprint(v.Field(i).Interface()), refreshSecret) {
			t.Fatalf("field %s holds the refresh token", v.Type().Field(i).Name)
		}
	}
	if a, err := p.Status(ctxT(t)); err != nil || a.Type != "chatgpt" || a.Email != "user@example.com" {
		t.Fatalf("Status = %+v, %v", a, err)
	}
	// Native stderr arrived redacted.
	if !strings.Contains(stderr.String(), "<redacted>") {
		t.Fatalf("stderr not forwarded: %q", stderr.String())
	}
	assertNoSecret(t, "stderr", stderr.String())
	if n := counter(home, "fake-server-request-answered"); n != 1 {
		t.Fatalf("server-initiated request answered %d times", n)
	}
}

func TestNativeInvocationIsPinnedAndScrubbed(t *testing.T) {
	p, home, _ := newFake(t)
	if err := p.Start(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var inv struct {
		Args []string
		Env  []string
		Cwd  string
	}
	b, err := os.ReadFile(filepath.Join(home, "fake-invocation.json"))
	if err != nil || json.Unmarshal(b, &inv) != nil {
		t.Fatal(err)
	}
	want := []string{"-c", `cli_auth_credentials_store="file"`, "-c", `forced_login_method="chatgpt"`, "app-server"}
	if !slices.Equal(inv.Args, want) {
		t.Fatalf("args %q, want %q", inv.Args, want)
	}
	if inv.Cwd != home {
		t.Fatalf("cwd %q", inv.Cwd)
	}
	var homes []string
	for _, kv := range inv.Env {
		key, value, _ := strings.Cut(kv, "=")
		switch key {
		case "OPENAI_API_KEY", "CODEX_API_KEY", "RUST_LOG":
			t.Fatalf("%s reached the native server", key)
		case "CODEX_HOME":
			homes = append(homes, value)
		}
	}
	if !slices.Equal(homes, []string{home}) {
		t.Fatalf("CODEX_HOME %q", homes)
	}
}

func TestFreshTokenIsNotRotated(t *testing.T) {
	p, home, _ := newFake(t)
	login(t, p)
	for range 5 {
		if _, err := p.SessionToken(ctxT(t), "s1", false, ""); err != nil {
			t.Fatal(err)
		}
	}
	// A second provider (after the first closes) reads the persisted token too.
	if n := counter(home, "fake-refreshes"); n != 0 {
		t.Fatalf("fresh token rotated %d times", n)
	}
}

func TestExpiringTokenRefreshes(t *testing.T) {
	p, home, _ := newFake(t, fakeTTL+"=1m") // inside the 5m skew
	login(t, p)
	tok, err := p.Tokens(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	if n := counter(home, "fake-refreshes"); n != 1 {
		t.Fatalf("refreshes %d, want 1", n)
	}
	assertNoSecret(t, "tokens", tok.String())
}

func TestForcedRefreshEvenBeforeExpiry(t *testing.T) {
	p, home, _ := newFake(t)
	login(t, p)
	first, err := p.SessionToken(ctxT(t), "s1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.SessionToken(ctxT(t), "s1", true, "acct-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken == first.AccessToken || second.Generation <= first.Generation {
		t.Fatalf("forced refresh did not issue a new token: %v -> %v", first, second)
	}
	if n := counter(home, "fake-refreshes"); n != 1 {
		t.Fatalf("refreshes %d, want 1", n)
	}
	// The same session's next 401 rotates again: it already holds the newest.
	if _, err := p.SessionToken(ctxT(t), "s1", true, "acct-1"); err != nil {
		t.Fatal(err)
	}
	if n := counter(home, "fake-refreshes"); n != 2 {
		t.Fatalf("refreshes %d, want 2", n)
	}
}

func TestConcurrentSessionRefreshesRotateOnce(t *testing.T) {
	p, home, _ := newFake(t, fakeRefreshSleep+"=100ms")
	login(t, p)
	const sessions = 12
	for i := range sessions {
		if _, err := p.SessionToken(ctxT(t), fmt.Sprint("s", i), false, ""); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make([]Tokens, sessions)
	errs := make([]error, sessions)
	for i := range sessions {
		wg.Go(func() { results[i], errs[i] = p.SessionToken(ctxT(t), fmt.Sprint("s", i), true, "acct-1") })
	}
	wg.Wait()
	for i := range sessions {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i].AccessToken != results[0].AccessToken {
			t.Fatalf("sessions received different tokens")
		}
	}
	if n := counter(home, "fake-refreshes"); n != 1 {
		t.Fatalf("%d concurrent 401s rotated %d times, want 1", sessions, n)
	}
}

func TestAccountContinuity(t *testing.T) {
	p, _, _ := newFake(t)
	login(t, p)
	if _, err := p.SessionToken(ctxT(t), "s1", false, "acct-other"); !errors.Is(err, ErrAccountChanged) {
		t.Fatalf("err = %v, want ErrAccountChanged", err)
	}
}

func TestRefreshWithoutNewTokenFails(t *testing.T) {
	p, _, _ := newFake(t, fakeSameToken+"=1")
	login(t, p) // login writes token n=0; the fake's refresh rewrites n=0
	if _, err := p.SessionToken(ctxT(t), "s1", false, ""); err != nil {
		t.Fatal(err)
	}
	_, err := p.SessionToken(ctxT(t), "s1", true, "")
	if !errors.Is(err, ErrRefreshFailed) || strings.Contains(err.Error(), ErrNotLoggedIn.Error()) {
		t.Fatalf("err = %v, want retryable ErrRefreshFailed", err)
	}
}

// A revoked refresh token makes Codex clear the account on account/read (no
// RPC error), which is the one case that needs a new login.
func TestRevokedRefreshNeedsLogin(t *testing.T) {
	p, _, _ := newFake(t, fakeRevokeOnRefresh+"=1")
	login(t, p)
	if _, err := p.SessionToken(ctxT(t), "s1", false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SessionToken(ctxT(t), "s1", true, ""); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("err = %v, want ErrNotLoggedIn", err)
	}
}

func TestAcquireHonorsDeadlineDuringLogin(t *testing.T) {
	p, _, _ := newFake(t, fakeHangLogin+"=1")
	loginCtx, cancelLogin := context.WithCancel(context.Background())
	shown := make(chan struct{})
	loginDone := make(chan error, 1)
	go func() { loginDone <- p.Login(loginCtx, func(string) { close(shown) }) }()
	<-shown
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.SessionToken(ctx, "s1", true, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("blocked %v past its deadline", d)
	}
	cancelLogin()
	if err := <-loginDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("login = %v", err)
	}
}

func TestLoginCancelIsNative(t *testing.T) {
	p, home, _ := newFake(t, fakeHangLogin+"=1")
	ctx, cancel := context.WithCancel(context.Background())
	err := p.Login(ctx, func(string) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := counter(home, "fake-cancels"); n != 1 {
		t.Fatalf("native cancels %d", n)
	}
}

func TestLogout(t *testing.T) {
	p, _, _ := newFake(t)
	login(t, p)
	if _, err := p.Tokens(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.Logout(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Tokens(ctxT(t)); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestRestartAfterNativeExit(t *testing.T) {
	p, home, _ := newFake(t, fakeCrashRead+"=1")
	if _, err := p.Status(ctxT(t)); err == nil {
		t.Fatal("first status should see the crash")
	} else {
		assertNoSecret(t, "exit error", err.Error())
	}
	if _, err := p.Status(ctxT(t)); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if n := counter(home, "fake-starts"); n != 2 {
		t.Fatalf("starts %d", n)
	}
}

func TestSingleOwnerAcrossProviders(t *testing.T) {
	p, home, _ := newFake(t)
	if err := p.Start(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	other, err := New(Config{Argv: []string{os.Args[0]}, Home: home, Env: []string{fakeEnv + "=1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Start(ctxT(t)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second owner = %v, want ErrBusy", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.Start(ctxT(t)); err != nil {
		t.Fatalf("after owner closed: %v", err)
	}
}

func TestCloseStopsProcessEvenIgnoringTerm(t *testing.T) {
	p, home, _ := newFake(t, fakeIgnoreTerm+"=1")
	if err := p.Start(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	var inv struct{ Pid int }
	b, _ := os.ReadFile(filepath.Join(home, "fake-invocation.json"))
	_ = json.Unmarshal(b, &inv)
	if inv.Pid == 0 {
		t.Fatal("no pid")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(inv.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("native server %d survived Close: %v", inv.Pid, err)
	}
	if _, err := p.Tokens(ctxT(t)); !errors.Is(err, ErrClosed) {
		t.Fatalf("after Close: %v", err)
	}
}

func TestInsecureHomeAndAuthFile(t *testing.T) {
	root := t.TempDir()
	loose := filepath.Join(root, "loose")
	if err := os.Mkdir(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Argv: []string{"codex"}, Home: loose}); !errors.Is(err, ErrInsecureHome) {
		t.Fatalf("loose home = %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Argv: []string{"codex"}, Home: link}); !errors.Is(err, ErrInsecureHome) {
		t.Fatalf("symlink home = %v", err)
	}
	if _, err := New(Config{Argv: []string{"codex"}, Home: "relative"}); err == nil {
		t.Fatal("relative home accepted")
	}

	home := filepath.Join(root, "home")
	if err := ensureHome(home); err != nil {
		t.Fatal(err)
	}
	writeAuth(home, 1, "acct-1", time.Hour)
	if err := os.Chmod(filepath.Join(home, authFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokens(home); !errors.Is(err, ErrInsecureHome) {
		t.Fatalf("world-readable auth.json = %v", err)
	}
	target := filepath.Join(root, "target.json")
	if err := os.Rename(filepath.Join(home, authFile), target); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(target, 0o600)
	if err := os.Symlink(target, filepath.Join(home, authFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokens(home); !errors.Is(err, ErrInsecureHome) {
		t.Fatalf("symlinked auth.json = %v", err)
	}
}

func TestParseAuthIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want error
	}{
		{"api key only", `{"OPENAI_API_KEY":"sk-abc"}`, ErrUnsupportedAuth},
		{"api key mode", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-abc","tokens":{"access_token":"a.b.c","account_id":"x"}}`, ErrUnsupportedAuth},
		{"empty", `{}`, ErrNotLoggedIn},
		{"no access token", `{"tokens":{"refresh_token":"` + refreshSecret + `","account_id":"x"}}`, errMalformed},
		{"no account", `{"tokens":{"access_token":"abc"}}`, errMalformed},
		{"control chars", `{"tokens":{"access_token":"a\nb","account_id":"x"}}`, errMalformed},
		{"truncated", `{"tokens":{"access_tok`, errMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseAuth([]byte(c.doc))
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if err != nil {
				assertNoSecret(t, "error", err.Error())
				if strings.Contains(err.Error(), "sk-abc") {
					t.Fatalf("error quotes the file: %v", err)
				}
			}
		})
	}
	tok, err := parseAuth([]byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"opaque","account_id":"acct","refresh_token":"` + refreshSecret + `"}}`))
	if err != nil || tok.AccessToken != "opaque" || tok.AccountID != "acct" || !tok.ExpiresAt.IsZero() {
		t.Fatalf("chatgpt doc = %v, %v", tok, err)
	}
}

func TestRedact(t *testing.T) {
	for _, s := range []string{
		"Authorization: Bearer abc.def",
		`{"refresh_token":"rt-1"}`,
		"key sk-proj-abcdefghijk",
		"jwt " + fakeJWT(map[string]any{"a": 1}),
		strings.Repeat("Z", 48),
	} {
		if r := redact(s); !strings.Contains(r, "<redacted>") || strings.Contains(r, "abc.def") || strings.Contains(r, "rt-1") ||
			strings.Contains(r, "sk-proj") || strings.Contains(r, "eyJ") || strings.Contains(r, "ZZZZ") {
			t.Fatalf("redact(%q) = %q", s, r)
		}
	}
}

func TestCloseInterruptsUnboundedLogin(t *testing.T) {
	p, _, _ := newFake(t, fakeHangLogin+"=1")
	shown := make(chan struct{})
	loginDone := make(chan error, 1)
	go func() { loginDone <- p.Login(context.Background(), func(string) { close(shown) }) }()
	<-shown
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked behind an unbounded login")
	}
	if err := <-loginDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("login = %v, want ErrClosed", err)
	}
}

// Pdeathsig follows the forking THREAD. A caller whose goroutine locked its
// thread and then exits (so the runtime terminates that thread) must not take
// the native server down with it.
func TestServerSurvivesExitOfCallersThread(t *testing.T) {
	p, _, _ := newFake(t)
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: this thread is retired on return
		done <- p.Start(ctxT(t))
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	pid := p.srv.cmd.Process.Pid
	time.Sleep(300 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil || !p.srv.alive() {
		t.Fatalf("native server died with its caller's thread: kill0=%v alive=%v", err, p.srv.alive())
	}
	if _, err := p.Status(ctxT(t)); err != nil {
		t.Fatalf("server unusable after caller thread exit: %v", err)
	}
}

// An abrupt owner death (SIGKILL, no Close) must take the native server with
// it, so the released owner lock never coexists with a live refresh writer.
func TestOwnerDeathKillsNativeServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("parent-death signal is Linux-only")
	}
	home := filepath.Join(t.TempDir(), "auth", "codex")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperOwnerEnv+"="+home)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "PID ") {
		_ = cmd.Process.Kill()
		t.Fatalf("helper owner: %q %v", line, err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PID ")))
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("native server not running: %v", err)
	}
	// While the owner lives, nobody else can own the home.
	if l, err := ownerLock(home); !errors.Is(err, ErrBusy) {
		if l != nil {
			_ = l.Close()
		}
		t.Fatalf("second owner = %v, want ErrBusy", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("native server outlived its SIGKILLed owner")
		}
		time.Sleep(20 * time.Millisecond)
	}
	l, err := ownerLock(home)
	if err != nil {
		t.Fatalf("owner lock not released after owner death: %v", err)
	}
	_ = l.Close()
}

func TestCancelledNativeLoginCannotPersistLateAccount(t *testing.T) {
	p, home, _ := newFake(t, fakeHangLogin+"=1", "AMUX_FAKE_LATE_LOGIN=1")
	writeAuth(home, 0, "original-account", time.Hour)
	before, err := os.ReadFile(filepath.Join(home, authFile))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = p.Login(ctx, func(string) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(home, authFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("late native callback replaced persisted account")
	}
	if p.srv != nil {
		t.Fatal("cancelled login writer still running")
	}
}

func TestRejectedNativeCompletionRollsBackAuthPersistence(t *testing.T) {
	p, home, _ := newFake(t)
	writeAuth(home, 0, "original-account", time.Hour)
	before, err := os.ReadFile(filepath.Join(home, authFile))
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("initiator revoked")
	err = p.Login(ctxT(t), nil, func() error { return denied })
	after, readErr := os.ReadFile(filepath.Join(home, authFile))
	if !errors.Is(err, denied) || readErr != nil || !bytes.Equal(before, after) || p.srv != nil {
		t.Fatal("rejected completion retained native write", err, readErr)
	}
}
