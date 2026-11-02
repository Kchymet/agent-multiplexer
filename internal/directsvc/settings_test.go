package directsvc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestSettingsDefaultOff(t *testing.T) {
	isolate(t)
	s, set, err := Load()
	if err != nil || set || s.Enabled {
		t.Fatalf("Load on a fresh host = %+v set=%v err=%v; want disabled and unset", s, set, err)
	}
}

func TestSettingsRoundTripPreservesOtherSections(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"keys":{"focus-rail":"ctrl+g"},"codex":{"control":"pty"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	want := enabledCfg
	want.AllowOrigins = []string{"https://harness.example"}
	if err := Save(want); err != nil {
		t.Fatal(err)
	}
	got, set, err := Load()
	if err != nil || !set {
		t.Fatalf("Load: set=%v err=%v", set, err)
	}
	if strings.Join(got.Args(), " ") != strings.Join(want.Args(), " ") || !got.Enabled {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	b, _ := os.ReadFile(core.ConfigPath())
	for _, keep := range []string{`"focus-rail"`, `"control"`, `"tls_key"`} {
		if !strings.Contains(string(b), keep) {
			t.Errorf("config.json lost %s:\n%s", keep, b)
		}
	}
	// The setting is not a credential store: nothing that looks like one is written.
	for _, never := range []string{"token", "secret", "BEGIN"} {
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(never)) {
			t.Errorf("config.json mentions %q:\n%s", never, b)
		}
	}
}

func TestSettingsUnknownKeyIsAnError(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"direct":{"enabled":true,"listn":"0.0.0.0:8443"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(); err == nil || !strings.Contains(err.Error(), "listn") {
		t.Fatalf("Load = %v, want an unknown-field error", err)
	}
}

func TestArgsMirrorServeFlags(t *testing.T) {
	s := Settings{Listen: "127.0.0.1:8443", Endpoints: []string{"https://a", "https://b"}, AllowOrigins: []string{"https://o"},
		TLSCert: "/c", TLSKey: "/k", AllowPublicBind: true, AccessTTL: "5m"}
	got := strings.Join(s.Args(), " ")
	want := "--listen 127.0.0.1:8443 --endpoint https://a --endpoint https://b --allow-origin https://o --tls-cert /c --tls-key /k --allow-public-bind --access-ttl 5m"
	if got != want {
		t.Fatalf("Args = %q\nwant  %q", got, want)
	}
}

// Renewing the certificate in place changes the fingerprint, so the supervisor
// restarts the listener onto the new certificate.
func TestFingerprintTracksCertificateRenewal(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	for _, p := range []string{cert, key} {
		if err := os.WriteFile(p, []byte("v1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := Settings{Enabled: true, Listen: "127.0.0.1:1", TLSCert: cert, TLSKey: key}
	a := Fingerprint(s)
	if b := Fingerprint(s); a != b {
		t.Fatalf("fingerprint not stable: %s vs %s", a, b)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(cert, later, later); err != nil {
		t.Fatal(err)
	}
	if Fingerprint(s) == a {
		t.Fatal("fingerprint ignored a renewed certificate")
	}
	s2 := s
	s2.Listen = "127.0.0.1:2"
	if Fingerprint(s2) == Fingerprint(s) {
		t.Fatal("fingerprint ignored a settings change")
	}
}

// ExecStarter runs the real child protocol against a stand-in binary: constant
// argv, output to the log, SIGTERM stops it, and a failing child's last line
// comes back as its exit reason.
func TestExecStarter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell stand-in")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "direct.log")

	runner := filepath.Join(dir, "amux-ok")
	writeScript(t, runner, "#!/bin/sh\necho \"argv: $*\"\ntrap 'echo stopping; exit 0' TERM\nwhile :; do sleep 0.05; done\n")
	p, err := ExecStarter(runner, logPath)(enabledCfg)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(logPath); return strings.Contains(string(b), "argv:") })
	if err := p.Terminate(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.Kill()
		t.Fatal("child did not stop on SIGTERM")
	}
	b, _ := os.ReadFile(logPath)
	if !strings.Contains(string(b), "argv: direct serve --managed") {
		t.Fatalf("child argv/log = %q, want the constant managed argv", b)
	}
	if strings.Contains(string(b), enabledCfg.TLSKey) {
		t.Fatalf("settings leaked into argv: %q", b)
	}
	if st, _ := os.Stat(logPath); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %04o, want 0600", st.Mode().Perm())
	}

	failing := filepath.Join(dir, "amux-fail")
	writeScript(t, failing, "#!/bin/sh\necho 'starting'\necho 'listen address 0.0.0.0:8443 is not a private or loopback address' >&2\nexit 1\n")
	p, err = ExecStarter(failing, logPath)(enabledCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err == nil || !strings.Contains(err.Error(), "not a private or loopback address") {
		t.Fatalf("Wait = %v, want the child's last line", err)
	}
}

func TestRotateLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct.log")
	if err := os.WriteFile(path, make([]byte, maxLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLog(path)
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("oversized log not rotated: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("log still present after rotation: %v", err)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
