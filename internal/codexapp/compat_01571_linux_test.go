//go:build linux

package codexapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"

	"github.com/gorilla/websocket"
)

const (
	codex01571ArchiveSHA256 = "e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2"
	codex01571BinarySHA256  = "3e2584f3f3829a43a0495011a1cecb2facbe64a2403e2b682351fd9c2983f970"
)

// TestCompatCodex01571ProtectedAttachReconnect is an opt-in compatibility
// regression for the exact official OpenAI x86_64 Linux release artifact. It
// uses an empty home, unshares the network, and sends no model turn. Besides the
// server/supervisor handshake, a second protected namespace performs the native
// TUI's initialize + paginated empty-thread resume. Then two concurrent Ensure
// calls recover a deliberately lost daemon-side WebSocket without replacing the
// serving process or changing the pinned thread.
func TestCompatCodex01571ProtectedAttachReconnect(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_01571_BIN")
	archive := os.Getenv("AMUX_TEST_CODEX_01571_ARCHIVE")
	if bin == "" || archive == "" {
		t.Skip("set AMUX_TEST_CODEX_01571_BIN and AMUX_TEST_CODEX_01571_ARCHIVE to the official 0.157.1 Linux artifact")
	}
	assertFileSHA256(t, archive, codex01571ArchiveSHA256)
	assertFileSHA256(t, bin, codex01571BinarySHA256)
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("codex --version: %v: %s", err, version)
	}
	if got := strings.TrimSpace(string(version)); got != "codex-cli 0.157.1" {
		t.Fatalf("codex version = %q, want codex-cli 0.157.1", got)
	}
	t.Logf("official rust-v0.157.1 archive sha256=%s binary sha256=%s", codex01571ArchiveSHA256, codex01571BinarySHA256)

	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap required: %v", err)
	}
	if os.Getenv("AMUX_JAIL") == "off" {
		t.Skip("AMUX_JAIL=off disables the namespace under test")
	}

	// Copy the verified binary into the isolated HOME so the production launcher
	// can grant that exact executable without restoring any ambient home tree.
	home := t.TempDir()
	testBin := filepath.Join(home, "codex")
	copyExecutable(t, bin, testBin)
	t.Setenv("HOME", home)
	// Exercise the real canonicalization boundary too: on the host the data path
	// resolves through this alias, while inside the private HOME the exact socket
	// directory is recreated as a bind destination at the lexical alias path.
	realData := t.TempDir()
	dataAlias := filepath.Join(home, "data-alias")
	if err := os.Symlink(realData, dataAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", dataAlias)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_CODEX_BIN", testBin)
	if err := amuxcfg.SetAuthMode("codex", amuxcfg.AuthHost); err != nil {
		t.Fatal(err)
	}
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	session := store.Session{
		ID: "compat-01571", RootID: "compat-root", Agent: "codex",
		Dir: filepath.Join(t.TempDir(), "work"),
	}
	if err := os.MkdirAll(session.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	authority, err := access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	grant, err := authority.EnsureSession(context.Background(), session.ID, session.Dir)
	if err != nil {
		t.Fatal(err)
	}
	spec := panespec.LaunchSpec{Session: session, Access: grant}
	dir, env, argv, endpoint, err := panespec.AppServerCommand(spec)
	if err != nil {
		t.Fatal(err)
	}
	argv = withoutExternalNetwork(t, argv)

	lifetime, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	manager := NewManager(lifetime, testBin)
	defer manager.Shutdown()
	ensure := func() (*Supervisor, error) {
		return manager.Ensure(lifetime, session.ID, dir, env, argv, endpoint, "", "", "")
	}
	sup, err := ensure()
	if err != nil {
		t.Fatalf("start protected 0.157.1 server: %v", err)
	}
	pid := supervisorPID(t, sup)
	threadID := sup.ThreadID()
	if threadID == "" {
		t.Fatal("0.157.1 initialize did not pin a thread")
	}
	runProtectedNativeBootstrap(t, spec, endpoint, threadID)

	sup.mu.Lock()
	oldRPC := sup.rpc
	sup.mu.Unlock()
	if oldRPC == nil {
		t.Fatal("supervisor has no control transport")
	}
	if err := oldRPC.close(); err != nil {
		t.Fatal(err)
	}

	results := make(chan *Supervisor, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			reconnected, err := ensure()
			results <- reconnected
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("reconnect Ensure %d: %v", i, err)
		}
		reconnected := <-results
		if reconnected != sup {
			t.Fatal("reconnect replaced the live supervisor")
		}
	}
	if got := supervisorPID(t, sup); got != pid {
		t.Fatalf("reconnect replaced serving process: pid %d -> %d", pid, got)
	}
	if got := sup.ThreadID(); got != threadID {
		t.Fatalf("reconnect changed pinned thread: %q -> %q", threadID, got)
	}
	runProtectedNativeBootstrap(t, spec, endpoint, threadID)
	t.Logf("0.157.1 protected initialize/native bootstrap/reconnect kept pid=%d thread=%s", pid, threadID)
}

// TestCompatCodex01571NativeBootstrapHelper runs only as the payload in the
// second protected namespace. It deliberately dials the rendezvous path
// directly, just as Codex --remote does; host-only alias translation is absent.
func TestCompatCodex01571NativeBootstrapHelper(t *testing.T) {
	endpoint := os.Getenv("AMUX_TEST_NATIVE_ENDPOINT")
	threadID := os.Getenv("AMUX_TEST_NATIVE_THREAD")
	if endpoint == "" || threadID == "" {
		t.Skip("protected subprocess helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	}}
	conn, _, err := dialer.DialContext(ctx, "ws://localhost", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetReadDeadline(deadline)
	_ = conn.SetWriteDeadline(deadline)
	call := func(id int, method string, params any) json.RawMessage {
		t.Helper()
		if err := conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for {
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := conn.ReadJSON(&reply); err != nil {
				t.Fatal(err)
			}
			if reply.ID != id {
				continue
			}
			if len(reply.Error) > 0 && string(reply.Error) != "null" {
				t.Fatalf("%s: %s", method, reply.Error)
			}
			return reply.Result
		}
	}
	call(1, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "amux-protected-native-bootstrap", "version": "0.157.1"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err := conn.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	resumed := call(2, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
	var result struct {
		Thread struct {
			ID    string            `json:"id"`
			Turns []json.RawMessage `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(resumed, &result); err != nil {
		t.Fatal(err)
	}
	if result.Thread.ID != threadID || len(result.Thread.Turns) != 0 {
		t.Fatalf("protected native bootstrap changed empty thread: %s", resumed)
	}
}

func runProtectedNativeBootstrap(t *testing.T, spec panespec.LaunchSpec, endpoint, threadID string) {
	t.Helper()
	dir, env, argv, err := panespec.AttachCommand(spec, endpoint, threadID)
	if err != nil {
		t.Fatal(err)
	}
	argv = withoutExternalNetwork(t, argv)
	separator := -1
	for i, arg := range argv {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+2 >= len(argv) {
		t.Fatalf("protected attach argv has no payload boundary: %v", argv)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv = append(append([]string(nil), argv[:separator+3]...), exe, "-test.run=^TestCompatCodex01571NativeBootstrapHelper$", "-test.v")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env, err = launchenv.Build(os.Environ(), env, launchenv.ForRuntime("codex"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(cmd.Env, "AMUX_TEST_NATIVE_ENDPOINT="+endpoint, "AMUX_TEST_NATIVE_THREAD="+threadID)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("protected native bootstrap: %v\n%s", err, output)
	}
}

func withoutExternalNetwork(t *testing.T, argv []string) []string {
	t.Helper()
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "bwrap") {
		t.Fatalf("compatibility fixture is not bubblewrap-protected: %v", argv)
	}
	return append([]string{argv[0], "--unshare-net"}, argv[1:]...)
}

func supervisorPID(t *testing.T, sup *Supervisor) int {
	t.Helper()
	sup.mu.Lock()
	defer sup.mu.Unlock()
	if sup.proc == nil || sup.proc.Process == nil || !sup.processRunningLocked() {
		t.Fatal("serving process is not running")
	}
	return sup.proc.Process.Pid
}

func assertFileSHA256(t *testing.T, path, want string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		t.Fatalf("sha256(%s) = %s, want %s", path, got, want)
	}
}

func copyExecutable(t *testing.T, source, target string) {
	t.Helper()
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
