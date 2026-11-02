package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/direct"
	"amux/internal/directsvc"
	"amux/internal/provider"
)

// writeTestCert writes a throwaway self-signed certificate and key into dir.
func writeTestCert(t *testing.T, dir string) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "amux.test"},
		DNSNames: []string{"amux.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	cert, key = filepath.Join(dir, "amux.pem"), filepath.Join(dir, "amux.key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// directCLI isolates the direct CLI: no daemon, no live server, a short wait.
type directCLI struct {
	t         *testing.T
	daemonUp  bool
	ensureErr error
	ensured   int
	server    *directServerStatus
	cert, key string
}

func newDirectCLI(t *testing.T) *directCLI {
	t.Helper()
	sandboxCLI(t)
	c := &directCLI{t: t}
	c.cert, c.key = writeTestCert(t, t.TempDir())
	oldEnsure, oldProbe, oldServer, oldBudget := directEnsureDaemon, startupProbe, directProbeServer, directWaitBudget
	directEnsureDaemon = func() error { c.ensured++; return c.ensureErr }
	startupProbe = func() error {
		if c.daemonUp {
			return nil
		}
		return errors.New("daemon offline (test)")
	}
	directProbeServer = func() *directServerStatus { return c.server }
	directWaitBudget = 2 * time.Second
	t.Cleanup(func() {
		directEnsureDaemon, startupProbe, directProbeServer, directWaitBudget = oldEnsure, oldProbe, oldServer, oldBudget
	})
	return c
}

func (c *directCLI) enableArgs(extra ...string) []string {
	return append([]string{"--listen", "100.64.0.5:8443", "--endpoint", "https://amux.test:8443",
		"--tls-cert", c.cert, "--tls-key", c.key}, extra...)
}

// supervise plays the daemon's supervisor: once the setting changes, it
// publishes the given state for the saved configuration.
func (c *directCLI) supervise(state string, lastErr string) {
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			s, _, err := directsvc.Load()
			if err == nil && (s.Enabled == (state != directsvc.StateDisabled)) {
				st := directsvc.Status{State: state, Enabled: s.Enabled, DaemonPID: os.Getpid(), LastError: lastErr,
					UpdatedAt: time.Now().Add(time.Second)}
				if s.Enabled {
					st.Fingerprint, st.PID = directsvc.Fingerprint(s), 4242
				}
				if state == directsvc.StateBackoff {
					st.NextRetry = time.Now().Add(time.Minute)
				}
				b := mustJSON(st)
				_ = os.MkdirAll(filepath.Dir(directsvc.StatusPath()), 0o755)
				_ = os.WriteFile(directsvc.StatusPath(), b, 0o644)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestDirectOffByDefault(t *testing.T) {
	newDirectCLI(t)
	out, err := captureOutput(t, func() error { return cmdDirectStatus(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "listener  off") || !strings.Contains(out, "direct.enabled off (default)") {
		t.Fatalf("status on a fresh host:\n%s", out)
	}
	if _, err := managedServeConfig(directsvc.Settings{}, false); err == nil {
		t.Fatal("managed serve ran from an absent setting")
	}
	if _, err := managedServeConfig(directsvc.Settings{Listen: "127.0.0.1:1"}, true); err == nil {
		t.Fatal("managed serve ran from a disabled setting")
	}
}

func TestValidateDirectSettings(t *testing.T) {
	cert, key := writeTestCert(t, t.TempDir())
	good := directsvc.Settings{Enabled: true, Listen: "192.168.1.20:8443", Endpoints: []string{"https://amux.home.example:8443"},
		TLSCert: cert, TLSKey: key}
	if err := validateDirectSettings(good); err != nil {
		t.Fatalf("good settings refused: %v", err)
	}
	for name, mut := range map[string]func(*directsvc.Settings){
		"public bind":        func(s *directsvc.Settings) { s.Listen = "0.0.0.0:8443" },
		"hostname listen":    func(s *directsvc.Settings) { s.Listen = "amux.local:8443" },
		"no endpoint":        func(s *directsvc.Settings) { s.Endpoints = nil },
		"http endpoint":      func(s *directsvc.Settings) { s.Endpoints = []string{"http://amux.test:8443"} },
		"non-canonical":      func(s *directsvc.Settings) { s.Endpoints = []string{"https://AMUX.test:443"} },
		"origin with path":   func(s *directsvc.Settings) { s.AllowOrigins = []string{"https://h.example/app"} },
		"no tls":             func(s *directsvc.Settings) { s.TLSCert, s.TLSKey = "", "" },
		"relative cert":      func(s *directsvc.Settings) { s.TLSCert = "amux.pem" },
		"unloadable cert":    func(s *directsvc.Settings) { s.TLSCert = key },
		"ttl over maximum":   func(s *directsvc.Settings) { s.AccessTTL = "1h" },
		"ttl not a duration": func(s *directsvc.Settings) { s.IdleTTL = "forever" },
	} {
		t.Run(name, func(t *testing.T) {
			s := good
			mut(&s)
			if err := validateDirectSettings(s); err == nil {
				t.Fatalf("accepted %+v", s)
			}
		})
	}
	// A public bind is possible only as a deliberate, saved choice.
	pub := good
	pub.Listen, pub.AllowPublicBind = "0.0.0.0:8443", true
	if err := validateDirectSettings(pub); err != nil {
		t.Fatalf("explicit public bind refused: %v", err)
	}
}

// An invalid enable saves nothing: the listener stays off.
func TestDirectEnableInvalidSavesNothing(t *testing.T) {
	c := newDirectCLI(t)
	_, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs("--listen", "0.0.0.0:8443")) })
	if err == nil || !strings.Contains(err.Error(), "nothing saved") {
		t.Fatalf("enable with a public bind = %v", err)
	}
	if _, set, _ := directsvc.Load(); set {
		t.Fatal("a refused enable saved the setting")
	}
	if c.ensured != 0 {
		t.Fatal("a refused enable started the daemon")
	}
}

// Enable saves absolute paths, starts the daemon through ensureDaemon, and
// reports ready only once the supervisor runs the saved configuration and the
// server answers.
func TestDirectEnableWaitsForReadiness(t *testing.T) {
	c := newDirectCLI(t)
	c.daemonUp = true
	c.server = &directServerStatus{ServerID: "sid-1", Endpoints: []string{"https://amux.test:8443"}}
	c.supervise(directsvc.StateRunning, "")
	out, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs()) })
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "listener running") || !strings.Contains(out, "sid-1") {
		t.Fatalf("enable output:\n%s", out)
	}
	if c.ensured != 1 {
		t.Fatalf("ensureDaemon called %d times, want 1", c.ensured)
	}
	s, _, _ := directsvc.Load()
	if !s.Enabled || !filepath.IsAbs(s.TLSCert) || s.Listen != "100.64.0.5:8443" {
		t.Fatalf("saved %+v", s)
	}
}

// A child that is alive but whose server never answers is not "ready".
func TestDirectEnableNotReadyUntilServerAnswers(t *testing.T) {
	c := newDirectCLI(t)
	c.daemonUp = true
	directWaitBudget = 500 * time.Millisecond
	c.supervise(directsvc.StateRunning, "")
	_, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs()) })
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("enable with a silent server = %v", err)
	}
}

func TestDirectEnableReportsStartFailure(t *testing.T) {
	c := newDirectCLI(t)
	c.daemonUp = true
	c.supervise(directsvc.StateBackoff, "another `amux direct serve` already owns the direct store")
	_, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs()) })
	if err == nil || !strings.Contains(err.Error(), "already owns the direct store") {
		t.Fatalf("enable with a failing child = %v", err)
	}
}

// --no-wait still starts the daemon; it skips only the readiness wait.
func TestDirectEnableNoWaitStillEnsuresDaemon(t *testing.T) {
	c := newDirectCLI(t)
	out, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs("--no-wait")) })
	if err != nil {
		t.Fatalf("enable --no-wait: %v", err)
	}
	if c.ensured != 1 || !strings.Contains(out, "amux direct status") {
		t.Fatalf("ensured=%d out=%q", c.ensured, out)
	}
}

// When the daemon cannot be started the setting is still saved, and the error
// says so and what to run.
func TestDirectEnableDaemonStartFailure(t *testing.T) {
	c := newDirectCLI(t)
	c.ensureErr = errors.New("automatic daemon startup requires a protected host credential")
	_, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs()) })
	if err == nil || !strings.Contains(err.Error(), "setting is saved") || !strings.Contains(err.Error(), "amux daemon start") {
		t.Fatalf("enable with no daemon = %v", err)
	}
	if s, _, _ := directsvc.Load(); !s.Enabled {
		t.Fatal("setting not saved")
	}
}

// Enable merges typed flags over the saved settings; a repeatable flag that is
// typed replaces the saved list.
func TestDirectEnableMerges(t *testing.T) {
	c := newDirectCLI(t)
	if _, err := captureOutput(t, func() error {
		return cmdDirectEnable(c.enableArgs("--allow-origin", "https://harness.example", "--no-wait"))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureOutput(t, func() error {
		return cmdDirectEnable([]string{"--endpoint", "https://other.test:8443", "--no-wait"})
	}); err != nil {
		t.Fatal(err)
	}
	s, _, _ := directsvc.Load()
	if s.Listen != "100.64.0.5:8443" || len(s.AllowOrigins) != 1 || len(s.Endpoints) != 1 || s.Endpoints[0] != "https://other.test:8443" {
		t.Fatalf("merged settings %+v", s)
	}
}

// Disable turns the listener off and keeps everything else; it is not
// revocation.
func TestDirectDisableKeepsSettings(t *testing.T) {
	c := newDirectCLI(t)
	if _, err := captureOutput(t, func() error { return cmdDirectEnable(c.enableArgs("--no-wait")) }); err != nil {
		t.Fatal(err)
	}
	c.daemonUp = true
	c.supervise(directsvc.StateDisabled, "")
	out, err := captureOutput(t, func() error { return cmdDirectDisable(nil) })
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "listener stopped") || !strings.Contains(out, "cloud enrollment policy are kept") {
		t.Fatalf("disable output:\n%s", out)
	}
	s, set, _ := directsvc.Load()
	if !set || s.Enabled || s.Listen == "" || s.TLSCert == "" {
		t.Fatalf("after disable: %+v", s)
	}
}

func TestServeManagedRefusesFlags(t *testing.T) {
	newDirectCLI(t)
	err := cmdDirectServe([]string{"--managed", "--listen", "0.0.0.0:1"})
	if err == nil || !strings.Contains(err.Error(), "saved setting") {
		t.Fatalf("serve --managed with flags = %v", err)
	}
	if err := cmdDirectServe([]string{"--managed"}); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("serve --managed while disabled = %v", err)
	}
}

func TestDirectPhase(t *testing.T) {
	cert, key := writeTestCert(t, t.TempDir())
	on := directsvc.Settings{Enabled: true, Listen: "127.0.0.1:8443", Endpoints: []string{"https://a.test"}, TLSCert: cert, TLSKey: key}
	fp := directsvc.Fingerprint(on)
	srv := &directServerStatus{ServerID: "sid"}
	running := &directsvc.Status{State: directsvc.StateRunning, Fingerprint: fp, DaemonPID: 1, PID: 2}
	for name, tc := range map[string]struct {
		s        directsvc.Settings
		set      bool
		m        *directsvc.Status
		daemonUp bool
		srv      *directServerStatus
		want     string
	}{
		"unset":                 {directsvc.Settings{}, false, nil, true, nil, directPhaseOff},
		"disabled":              {directsvc.Settings{Listen: "x"}, true, nil, true, nil, directPhaseOff},
		"daemon down":           {on, true, running, false, nil, directPhaseConfigured},
		"old daemon":            {on, true, nil, true, nil, directPhaseConfigured},
		"pending pickup":        {on, true, &directsvc.Status{State: directsvc.StateDisabled, DaemonPID: 1}, true, nil, directPhaseStarting},
		"child up, not serving": {on, true, running, true, nil, directPhaseStarting},
		"running":               {on, true, running, true, srv, directPhaseRunning},
		"backoff":               {on, true, &directsvc.Status{State: directsvc.StateBackoff, Fingerprint: fp, DaemonPID: 1, LastError: "bind: address in use"}, true, nil, directPhaseFailed},
		"invalid":               {on, true, &directsvc.Status{State: directsvc.StateInvalid, DaemonPID: 1, LastError: "unknown field"}, true, nil, directPhaseFailed},
	} {
		t.Run(name, func(t *testing.T) {
			if got, why := directPhase(tc.s, tc.set, tc.m, tc.daemonUp, tc.srv); got != tc.want {
				t.Fatalf("phase = %s (%s), want %s", got, why, tc.want)
			}
		})
	}
}

// The advertised state shown is what the provider registered with; anything
// else is labelled as inference, and a mismatch is flagged as stale.
func TestDirectProviderCoherence(t *testing.T) {
	cloudOn := &direct.CloudStatus{Enabled: true}
	srv := &directServerStatus{ServerID: "sid", Endpoints: []string{"https://a.test"}, Cloud: cloudOn}
	reg := provider.Status{State: provider.StateRegistered, ProviderID: "p1", DirectServerID: "sid", DirectEndpoints: []string{"https://a.test"}}
	noOffer := provider.Status{State: provider.StateRegistered, ProviderID: "p1"}

	v := directProviderCoherence(srv, true, reg, true)
	if !v.Observed || v.Stale || v.ObservedServerID != "sid" {
		t.Fatalf("live matching offer: %+v", v)
	}
	// The listener was disabled but the registration still carries the offer.
	v = directProviderCoherence(nil, true, reg, true)
	if !v.Observed || !v.Stale {
		t.Fatalf("offer outliving its server: %+v", v)
	}
	// The endpoints moved.
	moved := *srv
	moved.Endpoints = []string{"https://b.test"}
	if v = directProviderCoherence(&moved, true, reg, true); !v.Stale {
		t.Fatalf("moved endpoints not flagged: %+v", v)
	}
	// A dead provider's last record is never reported as an advertisement.
	if v = directProviderCoherence(srv, true, reg, false); v.Observed {
		t.Fatalf("dead provider reported as advertising: %+v", v)
	}
	// Server up, cloud on, provider registered without the offer yet: inferred.
	v = directProviderCoherence(srv, true, noOffer, true)
	if v.Observed || !v.Expected || !v.Stale || !strings.Contains(v.Note, "inferred") {
		t.Fatalf("pending offer: %+v", v)
	}
	// Listener off and provider not advertising: coherent, nothing claimed.
	v = directProviderCoherence(nil, true, noOffer, true)
	if v.Observed || v.Expected || v.Stale {
		t.Fatalf("listener off: %+v", v)
	}
}

func TestTailLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "direct.log")
	if err := os.WriteFile(p, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailLines(p, 2)
	if err != nil || strings.Join(got, ",") != "c,d" {
		t.Fatalf("tail = %v, %v", got, err)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
