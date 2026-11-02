package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/direct"
	"amux/internal/directsvc"
	"amux/internal/providercfg"
	"amux/internal/tailnet"
	"amux/internal/tailnet/tailnettest"
)

const tnHost = "monstrosity-40.example-tailnet.ts.net"

// tailnetCLI is directCLI plus a fake LocalAPI and a private CA standing in
// for the public roots. Nothing touches a real tailscaled or the host's
// amux config.
type tailnetCLI struct {
	*directCLI
	api  *tailnettest.LocalAPI
	ca   *tailnettest.CA
	port int
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func newTailnetCLI(t *testing.T) *tailnetCLI {
	t.Helper()
	c := &tailnetCLI{directCLI: newDirectCLI(t), ca: tailnettest.NewCA(t), port: freePort(t)}
	// 127.0.0.1 stands in for the node's Tailscale IPv4 so the port
	// preflight can bind it; derivation is the same.
	c.api = tailnettest.Start(t, tnHost+".", "fd7a:115c:a1e0::5", "127.0.0.1")
	t.Setenv(tailnet.EnvSocket, c.api.Socket)
	oldRoots, oldOwns, oldReach, oldCount := directTailnetRoots, managedOwnsAddr, directReachability, directClientCount
	directTailnetRoots = c.ca.Pool
	t.Cleanup(func() {
		directTailnetRoots, managedOwnsAddr, directReachability, directClientCount = oldRoots, oldOwns, oldReach, oldCount
	})
	return c
}

func (c *tailnetCLI) portArgs(extra ...string) []string {
	return append([]string{"--tailnet", "--port", strconv.Itoa(c.port), "--no-wait"}, extra...)
}

func (c *tailnetCLI) endpoint() string { return "https://" + tnHost + ":" + strconv.Itoa(c.port) }
func (c *tailnetCLI) listen() string   { return "127.0.0.1:" + strconv.Itoa(c.port) }

// existingPair writes ~/.config/amux/tls/<name>.crt/.key valid for d.
func (c *tailnetCLI) existingPair(name string, d time.Duration) (string, []byte) {
	c.t.Helper()
	dir := tailnetTLSDir()
	os.MkdirAll(dir, 0o700)
	cert, key := c.ca.Issue(c.t, tnHost, time.Now().Add(-time.Hour), time.Now().Add(d))
	cf := filepath.Join(dir, name+".crt")
	os.WriteFile(cf, cert, 0o644)
	os.WriteFile(filepath.Join(dir, name+".key"), key, 0o600)
	return cf, cert
}

func saveProvider(t *testing.T, orchestrator, serverName string) {
	t.Helper()
	if err := providercfg.Save(providercfg.Config{Orchestrator: orchestrator, TokenFile: "/nonexistent/token", ServerName: serverName}); err != nil {
		t.Fatal(err)
	}
}

func loadSaved(t *testing.T) directsvc.Settings {
	t.Helper()
	s, set, err := directsvc.Load()
	if err != nil || !set {
		t.Fatalf("settings set=%v err=%v", set, err)
	}
	return s
}

func TestDirectEnableTailnetReusesExistingCertAndDerives(t *testing.T) {
	c := newTailnetCLI(t)
	saveProvider(t, "harness.example.dev:443", "")
	src, srcPEM := c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	if !s.Enabled || s.Listen != c.listen() || len(s.Endpoints) != 1 || s.Endpoints[0] != c.endpoint() {
		t.Fatalf("saved %+v", s)
	}
	tn := s.Tailnet
	if tn == nil || tn.Port != c.port || tn.CertSource != tailnet.CertTailscale || tn.DNSName != tnHost || tn.ListenExplicit || tn.EndpointExplicit {
		t.Fatalf("tailnet %+v", tn)
	}
	store := tailnetStore(tnHost)
	if s.TLSCert != store.CertFile() || s.TLSKey != store.KeyFile() {
		t.Fatalf("tls %s %s", s.TLSCert, s.TLSKey)
	}
	if c.api.CertRequests() != 0 {
		t.Fatal("asked tailscaled for a certificate although a valid one existed")
	}
	if b, _ := os.ReadFile(src); string(b) != string(srcPEM) {
		t.Fatal("the existing server.crt was modified")
	}
	if len(s.AllowOrigins) != 1 || s.AllowOrigins[0] != "https://harness.example.dev" || tn.DefaultOrigin != "https://harness.example.dev" ||
		!strings.Contains(tn.OriginSource, "provider.toml") {
		t.Fatalf("origins %v default %q source %q", s.AllowOrigins, tn.DefaultOrigin, tn.OriginSource)
	}
	if c.ensured != 1 {
		t.Fatal("enable did not hand the listener to the daemon")
	}
}

func TestDirectEnableTailnetObtainsCertWhenNoneQualifies(t *testing.T) {
	c := newTailnetCLI(t)
	src, srcPEM := c.existingPair("server", 10*24*time.Hour) // under the 30-day threshold
	c.api.ServeCerts(t, c.ca, 90*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	if c.api.CertRequests() != 1 {
		t.Fatalf("%d certificate requests", c.api.CertRequests())
	}
	_, info, err := tailnetStore(tnHost).Load(time.Now())
	if err != nil || time.Until(info.NotAfter) < 80*24*time.Hour {
		t.Fatalf("store %+v %v", info, err)
	}
	if b, _ := os.ReadFile(src); string(b) != string(srcPEM) {
		t.Fatal("the near-expiry server.crt was overwritten")
	}
	// Re-enabling reuses the managed certificate.
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	if c.api.CertRequests() != 1 {
		t.Fatal("re-enable requested another certificate")
	}
}

func TestDirectEnableTailnetPrivateCAExistingPairNotImported(t *testing.T) {
	c := newTailnetCLI(t)
	// A pair from another (private) CA covering the name: not a managed
	// certificate; it is left alone and a fresh one is requested.
	other := tailnettest.NewCA(t)
	dir := tailnetTLSDir()
	os.MkdirAll(dir, 0o700)
	cert, key := other.Issue(t, tnHost, time.Now().Add(-time.Hour), time.Now().Add(300*24*time.Hour))
	os.WriteFile(filepath.Join(dir, "server.crt"), cert, 0o644)
	os.WriteFile(filepath.Join(dir, "server.key"), key, 0o600)
	c.api.ServeCerts(t, c.ca, 90*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	if c.api.CertRequests() != 1 {
		t.Fatal("private-CA pair imported as a managed certificate")
	}
}

func TestDirectEnableTailnetExplicitOverridesPrecedence(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	p2 := freePort(t)
	explicitListen := "127.0.0.1:" + strconv.Itoa(p2)
	if err := cmdDirectEnable(c.portArgs("--listen", explicitListen, "--allow-origin", "https://phone.example")); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	if s.Listen != explicitListen || !s.Tailnet.ListenExplicit || s.Endpoints[0] != c.endpoint() {
		t.Fatalf("saved %+v %+v", s, s.Tailnet)
	}
	if len(s.AllowOrigins) != 1 || s.AllowOrigins[0] != "https://phone.example" {
		t.Fatalf("origins %v (no provider: no default)", s.AllowOrigins)
	}
	// Plain re-enable keeps tailnet mode and the explicit listen, re-derives the rest.
	saveProvider(t, "tls:harness.example.dev:7443", "")
	if err := cmdDirectEnable([]string{"--no-wait"}); err != nil {
		t.Fatal(err)
	}
	s = loadSaved(t)
	if s.Tailnet == nil || s.Listen != explicitListen {
		t.Fatalf("re-enable lost tailnet mode or the explicit listen: %+v", s)
	}
	if got := strings.Join(s.AllowOrigins, ","); got != "https://harness.example.dev:7443,https://phone.example" {
		t.Fatalf("origins %s (derived keeps its port; explicit kept)", got)
	}
	// --listen= returns the address to derivation.
	if err := cmdDirectEnable([]string{"--no-wait", "--listen="}); err != nil {
		t.Fatal(err)
	}
	s = loadSaved(t)
	if s.Listen != c.listen() || s.Tailnet.ListenExplicit {
		t.Fatalf("listen %s explicit %v", s.Listen, s.Tailnet.ListenExplicit)
	}
	// --no-default-origin drops the derived one, keeps explicit.
	if err := cmdDirectEnable([]string{"--no-wait", "--tailnet", "--no-default-origin"}); err != nil {
		t.Fatal(err)
	}
	if s = loadSaved(t); strings.Join(s.AllowOrigins, ",") != "https://phone.example" {
		t.Fatalf("origins %v", s.AllowOrigins)
	}
	// --tailnet=false leaves tailnet mode (a plain configuration remains).
	if err := cmdDirectEnable([]string{"--no-wait", "--tailnet=false"}); err != nil {
		t.Fatal(err)
	}
	if s = loadSaved(t); s.Tailnet != nil {
		t.Fatal("still in tailnet mode")
	}
}

func TestDirectEnableTailnetCustomControlWithExplicitCert(t *testing.T) {
	c := newTailnetCLI(t)
	c.api.SetControlURL("https://headscale.example.org")
	c.api.Set("CertDomains", []string{})
	// Without a certificate: refused with a truthful reason, nothing saved.
	err := cmdDirectEnable(c.portArgs())
	if err == nil || !strings.Contains(err.Error(), "headscale.example.org") || strings.Contains(err.Error(), tailnet.AdminDNSURL) ||
		!strings.Contains(err.Error(), "nothing saved") {
		t.Fatalf("err %v", err)
	}
	if _, set, _ := directsvc.Load(); set {
		t.Fatal("saved on failure")
	}
	// With an explicit pair (here from a private CA): works, no cert API use.
	priv := tailnettest.NewCA(t)
	cert, key := priv.Issue(t, tnHost, time.Now().Add(-time.Hour), time.Now().Add(20*24*time.Hour))
	dir := t.TempDir()
	cf, kf := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cf, cert, 0o644)
	os.WriteFile(kf, key, 0o600)
	if err := cmdDirectEnable(c.portArgs("--tls-cert", cf, "--tls-key", kf)); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	if s.Tailnet.CertSource != tailnet.CertExplicit || s.TLSCert != cf || c.api.CertRequests() != 0 {
		t.Fatalf("saved %+v %+v requests %d", s, s.Tailnet, c.api.CertRequests())
	}
	// Explicit cert must cover the endpoint.
	wrong, wk := priv.Issue(t, "other.example", time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	os.WriteFile(cf, wrong, 0o644)
	os.WriteFile(kf, wk, 0o600)
	if err := cmdDirectEnable(c.portArgs("--tls-cert", cf, "--tls-key", kf)); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("err %v", err)
	}
}

func TestDirectEnableTailnetPreflightErrorsSaveNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*tailnetCLI)
		want  string
	}{
		"tailscaled down": {func(c *tailnetCLI) { t.Setenv(tailnet.EnvSocket, filepath.Join(t.TempDir(), "absent.sock")) }, "not reachable"},
		"logged out":      {func(c *tailnetCLI) { c.api.Set("BackendState", "NeedsLogin") }, "tailscale up"},
		"magicdns off":    {func(c *tailnetCLI) { c.api.SetMagicDNS(false) }, tailnet.AdminDNSURL},
		"https off":       {func(c *tailnetCLI) { c.api.Set("CertDomains", []string{}) }, "Enable HTTPS"},
		"not operator": {func(c *tailnetCLI) {
			c.api.OnCert(func(string, string) (int, string, http.Header) { return 403, "cert access denied", nil })
		}, "sudo tailscale set --operator=$USER"},
		"bad port": {func(c *tailnetCLI) { c.port = 70000 }, "not a TCP port"},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTailnetCLI(t)
			tc.setup(c)
			err := cmdDirectEnable(c.portArgs())
			if err == nil || !strings.Contains(strings.ReplaceAll(err.Error(), "enable HTTPS", "Enable HTTPS"), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if _, set, _ := directsvc.Load(); set {
				t.Fatal("saved on failure")
			}
			if c.ensured != 0 {
				t.Fatal("started the daemon on failure")
			}
		})
	}
}

func TestDirectEnableTailnetPortOccupied(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	ln, err := net.Listen("tcp", c.listen())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// An unrelated program: refused.
	if err := cmdDirectEnable(c.portArgs()); err == nil || !strings.Contains(err.Error(), "--port") {
		t.Fatalf("err %v", err)
	}
	// The daemon's own managed child on that address (re-enable): accepted.
	managedOwnsAddr = func(addr string) bool { return addr == c.listen() }
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
}

func TestManagedOwnsAddrNeedsMatchingMarkerAndServer(t *testing.T) {
	c := newTailnetCLI(t)
	s := directsvc.Settings{Enabled: true, Listen: "127.0.0.1:1"}
	fp := directsvc.Fingerprint(s)
	write := func(addr string, sid string) {
		os.MkdirAll(filepath.Dir(directsvc.StatusPath()), 0o755)
		os.WriteFile(directsvc.StatusPath(), mustJSON(directsvc.Status{State: directsvc.StateRunning, Enabled: true, PID: 77, Fingerprint: fp, DaemonPID: os.Getpid()}), 0o644)
		directsvc.WriteReady(directsvc.ReadyPath(), directsvc.Ready{PID: 77, ServerID: sid, Addr: addr, Fingerprint: fp})
	}
	write("127.0.0.1:9", "sid-1")
	c.server = &directServerStatus{ServerID: "sid-1"}
	if !managedOwnsAddr("127.0.0.1:9") {
		t.Fatal("the managed child's own address not recognised")
	}
	if managedOwnsAddr("127.0.0.1:10") {
		t.Fatal("a different address (effective drift) taken as owned")
	}
	c.server = &directServerStatus{ServerID: "manual"}
	if managedOwnsAddr("127.0.0.1:9") {
		t.Fatal("a manual server answering taken as the managed child")
	}
}

func TestPlainServeWithSavedTailnetMode(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	// Before any tailnet setting: --listen is still required.
	if err := cmdDirectServe(nil); err == nil || !strings.Contains(err.Error(), "--listen is required") {
		t.Fatalf("err %v", err)
	}
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	// Enabled: the daemon owns it; plain serve refuses rather than compete.
	if err := cmdDirectServe(nil); err == nil || !strings.Contains(err.Error(), "daemon-managed listener is enabled") {
		t.Fatalf("err %v", err)
	}
	// Disabled: plain serve picks up the saved tailnet configuration.
	s := loadSaved(t)
	s.Enabled = false
	directsvc.Save(s)
	got, err := savedTailnetManualConfig()
	if err != nil || got.Tailnet == nil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestApplyTailnetServeRederivesAtStart(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	// The node's address changed since enable.
	c.api.SetRunning(tnHost+".", "127.0.0.2")
	cfg, err := settingsServeConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	fp := directsvc.Fingerprint(s)
	run, err := applyTailnetServe(context.Background(), s, &cfg, true, fp, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.2:"+strconv.Itoa(c.port) || cfg.Endpoints[0] != c.endpoint() {
		t.Fatalf("effective %s %v", cfg.Listen, cfg.Endpoints)
	}
	if cfg.GetCertificate == nil || cfg.CertFile != "" {
		t.Fatal("certificate not served through the reloader")
	}
	if crt, _ := cfg.GetCertificate(nil); crt == nil || crt.Leaf.VerifyHostname(tnHost) != nil {
		t.Fatal("served certificate does not cover the name")
	}
	st, err := tailnet.ReadState(tailnetStatePath())
	if err != nil || st.Listen != cfg.Listen || st.Fingerprint != fp || st.PID != os.Getpid() || !st.Managed {
		t.Fatalf("state %+v %v", st, err)
	}
	// Status: effective (re-derived) first, flagged as differing from saved.
	v := tailnetView(s, &directsvc.Status{PID: os.Getpid()}, false)
	if v.Effective == nil || !v.Differs || v.CertNotAfter.IsZero() {
		t.Fatalf("view %+v", v)
	}
	// Explicit listen survives re-derivation.
	s.Tailnet.ListenExplicit, s.Listen = true, "127.0.0.1:"+strconv.Itoa(c.port)
	cfg2, _ := settingsServeConfig(s)
	if _, err := applyTailnetServe(context.Background(), s, &cfg2, true, fp, t.Logf); err != nil {
		t.Fatal(err)
	}
	if cfg2.Listen != s.Listen {
		t.Fatalf("explicit listen overridden: %s", cfg2.Listen)
	}
	// The renewer goroutine ends with the listener and cleans up its state.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { run(ctx); close(done) }()
	cancel()
	<-done
}

func TestApplyTailnetServeFailsRatherThanBindStale(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	c.api.Set("BackendState", "Stopped")
	cfg, _ := settingsServeConfig(s)
	if _, err := applyTailnetServe(context.Background(), s, &cfg, true, "", t.Logf); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("err %v", err)
	}
}

func TestApplyTailnetServeDriftReported(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	old := directDriftInterval
	directDriftInterval = 20 * time.Millisecond
	t.Cleanup(func() { directDriftInterval = old })
	cfg, _ := settingsServeConfig(s)
	run, err := applyTailnetServe(context.Background(), s, &cfg, false, "", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	c.api.SetRunning("renamed.example-tailnet.ts.net.", "127.0.0.1")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := tailnet.ReadState(tailnetStatePath()); err == nil && strings.Contains(st.Drift, "renamed.example-tailnet.ts.net") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("identity drift not reported in the state")
}

func TestDirectStatusTailnet(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs("--allow-origin", "https://phone.example")); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	cfg, _ := settingsServeConfig(s)
	fp := directsvc.Fingerprint(s)
	if _, err := applyTailnetServe(context.Background(), s, &cfg, true, fp, t.Logf); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(directsvc.StatusPath()), 0o755)
	os.WriteFile(directsvc.StatusPath(), mustJSON(directsvc.Status{State: directsvc.StateRunning, Enabled: true, PID: os.Getpid(), Fingerprint: fp, DaemonPID: os.Getpid(), UpdatedAt: time.Now()}), 0o644)
	directsvc.WriteReady(directsvc.ReadyPath(), directsvc.Ready{PID: os.Getpid(), ServerID: "sid-1", Addr: cfg.Listen, Fingerprint: fp})
	c.daemonUp, c.server = true, &directServerStatus{ServerID: "sid-1", Endpoints: cfg.Endpoints}
	directClientCount = func() (int, bool) { return 2, true }
	probed := ""
	directReachability = func(e string) *directReachProbe {
		probed = e
		return &directReachProbe{URL: e, OK: true, Result: "answered HTTP 404 with a valid certificate"}
	}
	out := captureStdout(t, func() {
		if err := cmdDirectStatus([]string{"--json"}); err != nil {
			t.Fatal(err)
		}
	})
	var r directStatusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r.Phase != directPhaseRunning || r.Clients == nil || *r.Clients != 2 || r.Tailnet == nil {
		t.Fatalf("report %+v", r)
	}
	if r.Tailnet.Effective == nil || r.Tailnet.Effective.Listen != c.listen() || r.Tailnet.CertNotAfter.IsZero() ||
		r.Tailnet.Reachability == nil || !r.Tailnet.Reachability.OK || probed != c.endpoint() {
		t.Fatalf("tailnet %+v", r.Tailnet)
	}
	if strings.Join(r.Setting.AllowOrigins, ",") != "https://phone.example" {
		t.Fatalf("origins %v", r.Setting.AllowOrigins)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatal("status printed key material")
	}
	human := captureStdout(t, func() { _ = cmdDirectStatus(nil) })
	for _, want := range []string{"tailnet   " + tnHost, "expires", "enrolled clients 2", "reach " + c.endpoint()} {
		if !strings.Contains(human, want) {
			t.Fatalf("status lacks %q:\n%s", want, human)
		}
	}
}

// Sanity: the managed setting written by enable --tailnet passes the same
// validation the child repeats, and the TLS key in amux's store is 0600.
func TestDirectEnableTailnetSavedSettingValidates(t *testing.T) {
	c := newTailnetCLI(t)
	c.existingPair("server", 200*24*time.Hour)
	if err := cmdDirectEnable(c.portArgs()); err != nil {
		t.Fatal(err)
	}
	s := loadSaved(t)
	if err := validateDirectSettings(s); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.TLSKey, core.ConfigDir()) {
		t.Fatalf("key %s outside amux's config dir", s.TLSKey)
	}
	real, _ := filepath.EvalSymlinks(s.TLSKey)
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", st.Mode().Perm())
	}
	if _, err := managedServeConfig(s, true); err != nil {
		t.Fatal(err)
	}
	_ = direct.CheckBind(s.Listen, false)
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	got := make(chan string)
	go func() { b, _ := io.ReadAll(r); got <- string(b) }()
	defer func() { os.Stdout = old }()
	run()
	w.Close()
	os.Stdout = old
	return <-got
}
