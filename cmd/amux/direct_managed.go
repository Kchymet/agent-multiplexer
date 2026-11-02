package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"amux/internal/direct"
	"amux/internal/directsvc"
	"amux/internal/provider"
	"amux/internal/providercfg"
)

// The daemon-managed direct listener: `amux direct enable` saves the
// configuration with enabled=true, and the daemon's supervisor
// (internal/directsvc) runs `amux direct serve --managed` from it for as long
// as the daemon runs. See docs/direct-access.md.

// directWaitBudget bounds how long enable/disable wait for the supervisor to
// apply the change. It covers one poll, a child start, and a SIGTERM grace.
var directWaitBudget = 15 * time.Second

// directSettingsFlags are the serve flags enable accepts, as pointers so it can
// tell which ones were typed (and merge only those over the saved setting).
type directSettingsFlags struct {
	listen, cert, key               *string
	accessTTL, idleTTL, absoluteTTL *string
	public                          *bool
	endpoints, origins              multiFlag
}

func (f *directSettingsFlags) register(fs *flag.FlagSet) {
	f.listen = fs.String("listen", "", "")
	f.cert = fs.String("tls-cert", "", "")
	f.key = fs.String("tls-key", "", "")
	f.public = fs.Bool("allow-public-bind", false, "")
	f.accessTTL = fs.String("access-ttl", "", "")
	f.idleTTL = fs.String("idle-ttl", "", "")
	f.absoluteTTL = fs.String("absolute-ttl", "", "")
	fs.Var(&f.endpoints, "endpoint", "")
	fs.Var(&f.origins, "allow-origin", "")
}

// apply overlays the typed flags onto s. A repeatable flag that was typed
// replaces the saved list rather than appending to it, so the command line is
// always the whole answer for that setting.
func (f *directSettingsFlags) apply(fs *flag.FlagSet, s *directsvc.Settings) error {
	var err error
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "listen":
			s.Listen = *f.listen
		case "tls-cert":
			s.TLSCert, err = absPath(*f.cert, err)
		case "tls-key":
			s.TLSKey, err = absPath(*f.key, err)
		case "allow-public-bind":
			s.AllowPublicBind = *f.public
		case "access-ttl":
			s.AccessTTL = *f.accessTTL
		case "idle-ttl":
			s.IdleTTL = *f.idleTTL
		case "absolute-ttl":
			s.AbsoluteTTL = *f.absoluteTTL
		case "endpoint":
			s.Endpoints = append([]string(nil), f.endpoints...)
		case "allow-origin":
			s.AllowOrigins = append([]string(nil), f.origins...)
		}
	})
	return err
}

// absPath makes a path absolute: the managed child runs from the daemon's
// working directory, not the one enable was typed in.
func absPath(p string, prior error) (string, error) {
	if prior != nil || p == "" {
		return p, prior
	}
	return filepath.Abs(p)
}

// settingsLifetimes parses the saved TTL strings.
func settingsLifetimes(s directsvc.Settings) (direct.Lifetimes, error) {
	var l direct.Lifetimes
	for _, f := range []struct {
		name string
		v    string
		dst  *time.Duration
	}{{"access_ttl", s.AccessTTL, &l.Access}, {"idle_ttl", s.IdleTTL, &l.Idle}, {"absolute_ttl", s.AbsoluteTTL, &l.Absolute}} {
		if f.v == "" {
			continue
		}
		d, err := time.ParseDuration(f.v)
		if err != nil {
			return l, fmt.Errorf("%s %q: %w", f.name, f.v, err)
		}
		*f.dst = d
	}
	return l, l.Validate()
}

// validateDirectSettings refuses a configuration the server would refuse,
// before it is saved: the daemon would otherwise only find out by restarting
// a failing child. It never relaxes a check direct.Serve makes — the child
// repeats all of them before it binds.
func validateDirectSettings(s directsvc.Settings) error {
	if s.Listen == "" {
		return errors.New("--listen is required (a private or loopback ip:port)")
	}
	if err := direct.CheckBind(s.Listen, s.AllowPublicBind); err != nil {
		return err
	}
	if len(s.Endpoints) == 0 {
		return errors.New("at least one --endpoint is required (the https origin clients dial)")
	}
	for _, e := range s.Endpoints {
		c, err := direct.CanonicalOrigin(e, false)
		if err != nil {
			return fmt.Errorf("endpoint: %w", err)
		}
		if c != e {
			return fmt.Errorf("endpoint %q is not canonical (use %q)", e, c)
		}
	}
	for _, o := range s.AllowOrigins {
		if c, err := direct.CanonicalOrigin(o, true); err != nil || c != o {
			return fmt.Errorf("allowed origin %q must be an exact canonical origin", o)
		}
	}
	if s.TLSCert == "" || s.TLSKey == "" {
		return errors.New("--tls-cert and --tls-key are required (HTTPS is mandatory, even on a private network)")
	}
	if !filepath.IsAbs(s.TLSCert) || !filepath.IsAbs(s.TLSKey) {
		return errors.New("tls_cert and tls_key must be absolute paths")
	}
	if _, err := tls.LoadX509KeyPair(s.TLSCert, s.TLSKey); err != nil {
		return fmt.Errorf("load TLS certificate: %w", err)
	}
	if _, err := settingsLifetimes(s); err != nil {
		return err
	}
	return nil
}

// managedServeConfig turns the saved setting into the serve configuration.
// The child refuses to run from a disabled setting: only an explicit enable
// may bring the listener up.
func managedServeConfig(s directsvc.Settings, set bool) (direct.ServeConfig, error) {
	if !set || !s.Enabled {
		return direct.ServeConfig{}, errors.New("the managed direct listener is not enabled (amux direct enable)")
	}
	life, err := settingsLifetimes(s)
	if err != nil {
		return direct.ServeConfig{}, err
	}
	return direct.ServeConfig{
		Listen: s.Listen, AllowPublicBind: s.AllowPublicBind, CertFile: s.TLSCert, KeyFile: s.TLSKey,
		Endpoints: append([]string(nil), s.Endpoints...), AllowOrigins: append([]string(nil), s.AllowOrigins...),
		Life: life,
	}, nil
}

func cmdDirectEnable(args []string) error {
	fs := flag.NewFlagSet("amux direct enable", flag.ContinueOnError)
	var f directSettingsFlags
	f.register(fs)
	noWait := fs.Bool("no-wait", false, "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	s, _, err := directsvc.Load()
	if err != nil {
		return err
	}
	if err := f.apply(fs, &s); err != nil {
		return err
	}
	s.Enabled = true
	if err := validateDirectSettings(s); err != nil {
		return fmt.Errorf("direct enable: %w (nothing saved)", err)
	}
	if s.AllowPublicBind {
		fmt.Fprintf(os.Stderr, "WARNING: allow_public_bind is set: %s may be reachable beyond your private networks.\n", s.Listen)
	}
	if st, err := os.Stat(s.TLSKey); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: TLS key %s is mode %04o; consider chmod 600 (amux does not change it)\n", s.TLSKey, st.Mode().Perm())
	}
	since := time.Now()
	if err := directsvc.Save(s); err != nil {
		return err
	}
	fmt.Printf("direct.enabled = on  (saved; listen %s, endpoints %s)\n", s.Listen, strings.Join(s.Endpoints, ", "))
	// The daemon owns the listener, so a stopped daemon is started the same way
	// any amux command starts it. --no-wait skips only the readiness wait.
	if err := directEnsureDaemon(); err != nil {
		return fmt.Errorf("the setting is saved, but the daemon could not be started: %w\nstart it with amux daemon start; the listener starts with it (amux direct status shows progress)", err)
	}
	if *noWait {
		fmt.Println("the daemon applies it within a few seconds; check with amux direct status")
		return nil
	}
	return waitManaged(directsvc.Fingerprint(s), true, since)
}

// directEnsureDaemon starts the daemon if none is answering, through the
// ordinary ensureDaemon path (and its policy: never from inside an agent
// session, never without a protected host credential). A variable so tests
// can run enable without a daemon.
var directEnsureDaemon = func() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return ensureDaemon(self)
}

func cmdDirectDisable(args []string) error {
	fs := flag.NewFlagSet("amux direct disable", flag.ContinueOnError)
	noWait := fs.Bool("no-wait", false, "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	s, set, err := directsvc.Load()
	if err != nil {
		return err
	}
	since := time.Now()
	if set && s.Enabled {
		s.Enabled = false
		if err := directsvc.Save(s); err != nil {
			return err
		}
	}
	fmt.Println("direct.enabled = off  (saved; listen/TLS settings kept for the next enable)")
	fmt.Println("Enrolled clients and the cloud enrollment policy are kept; they cannot connect while the listener is off.")
	if *noWait {
		return nil
	}
	return waitManaged("", false, since)
}

// waitManaged waits for the daemon's supervisor to apply the change. Enabling
// is ready only when the server itself answers on its admin socket — a child
// process that is merely alive may still be failing to bind. With no daemon
// answering there is nothing to wait for: the setting applies when it starts.
func waitManaged(fp string, running bool, since time.Time) error {
	if err := startupProbe(); err != nil {
		if running {
			fmt.Println("the daemon is not running; the listener starts when it does (amux daemon start)")
		}
		return nil
	}
	deadline := time.Now().Add(directWaitBudget)
	var last directsvc.Status
	for time.Now().Before(deadline) {
		st, err := directsvc.ReadStatus(directsvc.StatusPath())
		if err == nil && !st.UpdatedAt.Before(since) {
			last = st
			switch {
			case running && st.Fingerprint == fp && st.State == directsvc.StateRunning:
				// Ready means this child's own server is up — not merely that some
				// server answers the admin socket, which a manual serve can do
				// while the new child is still about to fail on the store lock.
				if r := readManagedReady(); st.Serving(r) {
					fmt.Printf("listener running (pid %d, server_id %s on %s) — amux direct status for details\n", st.PID, r.ServerID, r.Addr)
					return nil
				}
			case running && st.Fingerprint == fp && st.State == directsvc.StateBackoff && st.LastError != "":
				return fmt.Errorf("the daemon could not start the listener: %s (it retries with backoff; see amux direct logs)", st.LastError)
			case running && st.State == directsvc.StateInvalid:
				return fmt.Errorf("the daemon cannot read the setting: %s", st.LastError)
			case !running && st.State == directsvc.StateDisabled:
				fmt.Println("listener stopped")
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if last.State == "" {
		return fmt.Errorf("the running daemon has not picked up the setting within %s; it may predate managed direct support — run amux daemon restart to load the current binary", directWaitBudget)
	}
	return fmt.Errorf("the listener is not ready after %s (supervisor: %s; see amux direct status and amux direct logs)", directWaitBudget, last)
}

// directProbeServer is probeDirectServer, replaceable in tests.
var directProbeServer = probeDirectServer

// readManagedReady reads the managed child's readiness marker, if any.
func readManagedReady() *directsvc.Ready {
	r, err := directsvc.ReadReady(directsvc.ReadyPath())
	if err != nil {
		return nil
	}
	return &r
}

// Phases `amux direct status` reports for the managed listener.
const (
	directPhaseOff        = "off"
	directPhaseConfigured = "configured"
	directPhaseStarting   = "starting"
	directPhaseRunning    = "running"
	directPhaseFailed     = "failed"
)

// directPhase condenses the setting, the supervisor's record and the live
// server into one answer, with the reason when it is not simply running.
func directPhase(s directsvc.Settings, set bool, m *directsvc.Status, ready *directsvc.Ready, daemonUp bool, srv *directServerStatus) (phase, why string) {
	if !set || !s.Enabled {
		return directPhaseOff, ""
	}
	if !daemonUp {
		return directPhaseConfigured, "the daemon is not running; the listener starts with it (amux daemon start)"
	}
	if m == nil || m.DaemonPID == 0 {
		return directPhaseConfigured, "the daemon has not reported on the listener; it may predate managed direct support (amux daemon restart)"
	}
	if m.Fingerprint != directsvc.Fingerprint(s) && m.State != directsvc.StateInvalid {
		return directPhaseStarting, "the daemon has not applied the latest settings yet"
	}
	switch m.State {
	case directsvc.StateRunning:
		switch {
		case m.Serving(ready) && srv != nil && srv.ServerID == ready.ServerID:
			return directPhaseRunning, ""
		case m.Serving(ready):
			return directPhaseStarting, "the managed server reported ready but is not answering its admin socket"
		case srv != nil:
			return directPhaseStarting, "another direct server (a manual amux direct serve?) answers for the store; the managed child cannot own it"
		}
		return directPhaseStarting, "the listener process is up but its server is not ready yet"
	case directsvc.StateBackoff:
		return directPhaseFailed, m.LastError + " (retrying at " + m.NextRetry.Format(time.RFC3339) + ")"
	case directsvc.StateInvalid:
		return directPhaseFailed, "setting unreadable: " + m.LastError
	}
	return directPhaseStarting, "supervisor state " + m.State
}

// directStatusReport is `amux direct status --json`.
type directStatusReport struct {
	Phase    string              `json:"phase"` // off | configured | starting | running | failed
	Reason   string              `json:"reason,omitempty"`
	Setting  directsvc.Settings  `json:"setting"`
	Set      bool                `json:"set"`
	Managed  *directsvc.Status   `json:"managed,omitempty"`
	Ready    *directsvc.Ready    `json:"ready,omitempty"`
	Server   *directServerStatus `json:"server,omitempty"`
	Provider directProviderView  `json:"provider"`
}

type directServerStatus struct {
	ServerID  string              `json:"server_id"`
	Endpoints []string            `json:"endpoints"`
	Cloud     *direct.CloudStatus `json:"cloud,omitempty"`
}

// directProviderView separates what the provider actually advertised (its
// latest accepted registration, from its status file) from what this host
// would expect it to advertise. Only Observed* is a fact about the
// orchestrator; Expected is an inference.
type directProviderView struct {
	DirectEnroll      bool     `json:"direct_enroll"` // provider configured to offer direct-enroll-v1
	State             string   `json:"state,omitempty"`
	ProviderID        string   `json:"provider_id,omitempty"`
	Observed          bool     `json:"observed_advertised"` // the live registration carries an offer
	ObservedServerID  string   `json:"observed_server_id,omitempty"`
	ObservedEndpoints []string `json:"observed_endpoints,omitempty"`
	Expected          bool     `json:"expected_advertised"` // inferred: listener up, cloud enabled, provider configured
	Stale             bool     `json:"stale,omitempty"`     // observed differs from expected; the provider re-registers within about a minute
	Note              string   `json:"note,omitempty"`
}

func cmdDirectStatus(args []string) error {
	asJSON := len(args) == 1 && args[0] == "--json"
	if len(args) > 0 && !asJSON {
		return errors.New("usage: amux direct status [--json]")
	}
	r := directStatusReport{}
	var err error
	if r.Setting, r.Set, err = directsvc.Load(); err != nil {
		return err
	}
	if st, err := directsvc.ReadStatus(directsvc.StatusPath()); err == nil {
		r.Managed = &st
	}
	r.Server = directProbeServer()
	daemonUp := startupProbe() == nil
	r.Ready = readManagedReady()
	r.Phase, r.Reason = directPhase(r.Setting, r.Set, r.Managed, r.Ready, daemonUp, r.Server)
	cfg, cfgErr := providercfg.Load()
	pst, pstErr := provider.ReadStatus(provider.StatusPath())
	r.Provider = directProviderCoherence(r.Server, cfg.DirectEnroll && cfgErr == nil, pst, pstErr == nil && processAlive(pst.PID))
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(r)
	}
	printDirectStatus(r)
	return nil
}

// probeDirectServer asks a running server (managed or manual) for its
// identity over the host-proven admin socket. It never falls back to opening
// the store offline: status must not take the lock a starting server needs.
func probeDirectServer() *directServerStatus {
	cred, err := hostCredential()
	if err != nil {
		return nil
	}
	root, err := directRoot()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := direct.AdminCall(ctx, root, cred, direct.AdminRequest{Op: direct.OpCloudStatus})
	if err != nil || resp.ServerID == "" {
		return nil
	}
	return &directServerStatus{ServerID: resp.ServerID, Endpoints: resp.Endpoints, Cloud: resp.Cloud}
}

// directProviderCoherence compares the offer the provider registered with
// against the offer the running direct server would make now.
func directProviderCoherence(srv *directServerStatus, configured bool, st provider.Status, live bool) directProviderView {
	v := directProviderView{DirectEnroll: configured}
	registered := live && st.State == provider.StateRegistered
	if live {
		v.State, v.ProviderID = st.State, st.ProviderID
	}
	if registered && st.DirectServerID != "" {
		v.Observed, v.ObservedServerID, v.ObservedEndpoints = true, st.DirectServerID, st.DirectEndpoints
	}
	cloud := srv != nil && srv.Cloud != nil && srv.Cloud.Enabled
	v.Expected = configured && registered && cloud
	switch {
	case v.Observed && (srv == nil || !cloud || srv.ServerID != st.DirectServerID || !slices.Equal(srv.Endpoints, st.DirectEndpoints)):
		v.Stale = true
		v.Note = "the registered offer no longer matches the direct server; the provider re-registers within about a minute"
	case v.Observed:
		v.Note = "advertised in the provider's current registration"
	case !live || st.State != provider.StateRegistered:
		v.Note = "not advertised: no registered provider on this host"
	case srv == nil:
		v.Note = "not advertised: no direct server is answering"
	case !cloud:
		v.Note = "not advertised: cloud enrollment is off (amux direct cloud enable)"
	case !configured:
		v.Note = "not advertised: the provider is not configured to offer it (amux provide install --direct-enroll)"
	default:
		v.Stale = true
		v.Note = "not advertised yet (inferred: it should be); the provider re-registers within about a minute"
	}
	return v
}

func printDirectStatus(r directStatusReport) {
	s := r.Setting
	line := "listener  " + r.Phase
	if r.Reason != "" {
		line += " — " + r.Reason
	}
	fmt.Println(line)
	switch {
	case !r.Set:
		fmt.Println("setting   direct.enabled off (default) — amux direct enable --listen … --endpoint … --tls-cert … --tls-key …")
	default:
		fmt.Printf("setting   direct.enabled %s · listen %s · endpoints %s\n", onOff(s.Enabled), s.Listen, strings.Join(s.Endpoints, ", "))
		fmt.Printf("          tls %s · %s\n", s.TLSCert, s.TLSKey)
		if len(s.AllowOrigins) > 0 {
			fmt.Printf("          allow-origin %s\n", strings.Join(s.AllowOrigins, ", "))
		}
		if s.AllowPublicBind {
			fmt.Println("          WARNING allow_public_bind is set")
		}
	}
	if m := r.Managed; m != nil {
		fmt.Printf("daemon    supervisor (pid %d): %s · restarts %d\n", m.DaemonPID, m, m.Restarts)
		if m.LastError != "" {
			fmt.Printf("          last error: %s\n", m.LastError)
		}
	}
	if r.Server == nil {
		fmt.Println("server    not answering")
	} else {
		fmt.Printf("server    server_id %s · endpoints %s\n", r.Server.ServerID, strings.Join(r.Server.Endpoints, ", "))
		if r.Managed == nil || !r.Managed.Serving(r.Ready) || r.Ready.ServerID != r.Server.ServerID {
			fmt.Println("          (not the daemon's child: a manually run amux direct serve owns the store)")
		}
		if r.Server.Cloud != nil {
			fmt.Print("          ")
			printCloudStatus(*r.Server.Cloud)
		}
	}
	mark := "·"
	switch {
	case r.Provider.Stale:
		mark = "⚠"
	case r.Provider.Observed:
		mark = "✓"
	}
	fmt.Printf("provider  %s direct-enroll: %s\n", mark, r.Provider.Note)
	if r.Provider.Observed {
		fmt.Printf("          registered offer: server_id %s · %s\n", r.Provider.ObservedServerID, strings.Join(r.Provider.ObservedEndpoints, ", "))
	}
	fmt.Printf("logs      %s (amux direct logs)\n", directsvc.LogPath())
}

func cmdDirectLogs(args []string) error {
	fs := flag.NewFlagSet("amux direct logs", flag.ContinueOnError)
	n := fs.Int("n", 50, "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: amux direct logs [-n lines]")
	}
	lines, err := tailLines(directsvc.LogPath(), *n)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "no managed listener log yet (%s)\n", directsvc.LogPath())
		return nil
	}
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if n <= 0 {
		return nil, nil
	}
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	return ring, sc.Err()
}

// reportDirect prints doctor's Direct section: the persisted setting and, when
// it is on, the managed listener's phase. It never probes beyond the status
// file and the admin socket, and never opens the direct store.
func reportDirect() {
	s, set, err := directsvc.Load()
	if err != nil {
		fmt.Printf("  ✗ setting   %v\n", err)
		return
	}
	if !set || !s.Enabled {
		fmt.Printf("  · listener  off — amux direct enable to have the daemon run it\n")
		return
	}
	var m *directsvc.Status
	if st, err := directsvc.ReadStatus(directsvc.StatusPath()); err == nil {
		m = &st
	}
	phase, why := directPhase(s, set, m, readManagedReady(), startupProbe() == nil, directProbeServer())
	mark := map[string]string{directPhaseRunning: "✓", directPhaseFailed: "✗"}[phase]
	if mark == "" {
		mark = "⚠"
	}
	line := fmt.Sprintf("  %s listener  %s · %s", mark, phase, s.Listen)
	if why != "" {
		line += " — " + why
	}
	fmt.Println(line)
}
