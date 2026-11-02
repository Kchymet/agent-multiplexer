package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/direct"
	"amux/internal/directsvc"
	"amux/internal/providercfg"
	"amux/internal/tailnet"
)

// Tailnet mode: `amux direct enable --tailnet` derives the listen address,
// endpoint and certificate from this machine's tailnet identity (tailscaled's
// LocalAPI) and saves them into the ordinary daemon-managed setting. The
// daemon's supervised child stays the only owner of the listener: it
// re-derives the address at each start and renews and reloads the
// certificate in place while serving. See docs/direct-access.md ("Tailnet
// mode").

// tailnetStatePath is where the process serving in tailnet mode publishes its
// effective address and certificate state.
func tailnetStatePath() string { return filepath.Join(core.StateDir(), "direct-tailnet.json") }

// tailnetTLSDir is amux's TLS directory, where an existing pair is looked for
// and the managed store lives.
func tailnetTLSDir() string { return filepath.Join(core.ConfigDir(), "tls") }

// directTailnetNow is the clock tailnet mode uses (tests pin it).
var directTailnetNow = time.Now

// directTailnetRoots verifies managed certificates' chains (nil: the system
// roots; tests substitute a private root).
var directTailnetRoots *x509.CertPool

// directDriftInterval is how often the serving process re-reads its tailnet
// identity to notice a changed MagicDNS name or address.
var directDriftInterval = 5 * time.Minute

func tailnetStore(host string) tailnet.CertStore {
	return tailnet.CertStore{Dir: tailnet.StoreDir(core.ConfigDir(), host), Host: host, Roots: directTailnetRoots}
}

// tailnetFlags are enable's tailnet-mode flags.
type tailnetFlags struct {
	on        *bool
	port      *int
	socket    *string
	noDefault *bool
}

func (t *tailnetFlags) register(fs *flag.FlagSet) {
	t.on = fs.Bool("tailnet", false, "")
	t.port = fs.Int("port", 0, "")
	t.socket = fs.String("tailscale-socket", "", "")
	t.noDefault = fs.Bool("no-default-origin", false, "")
}

func visited(fs *flag.FlagSet, names ...string) bool {
	hit := false
	fs.Visit(func(f *flag.Flag) {
		if slices.Contains(names, f.Name) {
			hit = true
		}
	})
	return hit
}

// tailnetWanted reports whether this enable runs in tailnet mode: --tailnet
// typed, or tailnet mode already saved and not turned off with --tailnet=false.
func tailnetWanted(fs *flag.FlagSet, t *tailnetFlags, s directsvc.Settings) (bool, error) {
	if visited(fs, "tailnet") {
		if !*t.on && visited(fs, "port", "tailscale-socket", "no-default-origin") {
			return false, errors.New("--port, --tailscale-socket and --no-default-origin apply only with --tailnet")
		}
		return *t.on, nil
	}
	if s.Tailnet != nil {
		return true, nil
	}
	if visited(fs, "port", "tailscale-socket", "no-default-origin") {
		return false, errors.New("--port, --tailscale-socket and --no-default-origin apply only with --tailnet")
	}
	return false, nil
}

// prepareTailnet fills s (already carrying any typed serve flags) for tailnet
// mode. Precedence for each of listen, endpoint and certificate: a flag typed
// now, then an explicit value saved earlier, then derivation from tailscaled.
// Typing an empty value (--listen= , --endpoint= , --tls-cert= --tls-key=)
// returns that setting to derivation. Nothing is saved here; on success the
// only side effect is a certificate added to amux's managed store.
func prepareTailnet(ctx context.Context, fs *flag.FlagSet, t *tailnetFlags, s *directsvc.Settings, out io.Writer) error {
	now := directTailnetNow()
	tn := directsvc.TailnetSettings{}
	if s.Tailnet != nil {
		tn = *s.Tailnet
	}
	if visited(fs, "port") {
		if *t.port < 1 || *t.port > 65535 {
			return fmt.Errorf("--port %d is not a TCP port", *t.port)
		}
		tn.Port = *t.port
	}
	if tn.Port == 0 {
		tn.Port = tailnet.DefaultPort
	}
	if visited(fs, "tailscale-socket") {
		tn.Socket = *t.socket
	}
	if visited(fs, "listen") {
		tn.ListenExplicit = s.Listen != ""
	}
	if visited(fs, "endpoint") {
		s.Endpoints = slices.DeleteFunc(s.Endpoints, func(e string) bool { return e == "" })
		tn.EndpointExplicit = len(s.Endpoints) > 0
	}
	if visited(fs, "tls-cert", "tls-key") {
		switch {
		case s.TLSCert != "" && s.TLSKey != "":
			tn.CertSource = tailnet.CertExplicit
		case s.TLSCert == "" && s.TLSKey == "":
			tn.CertSource = tailnet.CertTailscale
		default:
			return errors.New("give --tls-cert and --tls-key together (or both empty to let amux obtain the certificate)")
		}
	}
	if tn.CertSource == "" {
		tn.CertSource = tailnet.CertTailscale
	}

	client := &tailnet.Client{Socket: tailnet.Socket(tn.Socket)}
	needIdentity := !tn.ListenExplicit || !tn.EndpointExplicit || tn.CertSource == tailnet.CertTailscale
	node, derr := tailnet.Discover(ctx, client)
	if derr != nil {
		if needIdentity {
			return derr
		}
		fmt.Fprintf(out, "note: %v\n  (continuing: listen, endpoint and certificate are all explicit)\n", derr)
	}
	if !tn.ListenExplicit {
		s.Listen = node.Listen(tn.Port)
	}
	if !tn.EndpointExplicit {
		s.Endpoints = []string{node.Endpoint(tn.Port)}
	}
	tn.DNSName = node.DNSName
	if node.DNSName != "" {
		ctrl := "Tailscale"
		if !node.Hosted() {
			ctrl = "custom control server " + node.ControlURL + " (not Tailscale's hosted control plane)"
		}
		fmt.Fprintf(out, "tailnet   %s · %s · %s · %s\n", node.DNSName, node.IP, displayOr(node.Tailnet, "tailnet"), ctrl)
	}
	hosts := tailnet.EndpointHosts(s.Endpoints)
	if len(hosts) == 0 {
		return errors.New("no endpoint host to serve a certificate for")
	}

	switch tn.CertSource {
	case tailnet.CertExplicit:
		_, info, err := tailnet.LoadPairFor(s.TLSCert, s.TLSKey, hosts, now)
		if err != nil {
			return fmt.Errorf("--tls-cert/--tls-key: %w", err)
		}
		fmt.Fprintf(out, "cert      explicit %s (valid until %s); reloaded in place when the files change\n", s.TLSCert, info.NotAfter.Format("2006-01-02"))
		if info.NotAfter.Sub(now) < tailnet.RenewBefore {
			fmt.Fprintf(out, "warning: the certificate expires in under 30 days and amux does not renew explicit certificates\n")
		}
	default:
		for _, h := range hosts {
			if h != node.DNSName {
				return fmt.Errorf("endpoint host %s is not this node's MagicDNS name %s, which is the only name a tailnet certificate covers; pass --tls-cert and --tls-key for it", h, node.DNSName)
			}
		}
		info, note, err := ensureTailnetCert(ctx, client, node, now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "cert      %s (valid until %s)\n", note, info.NotAfter.Format("2006-01-02"))
		s.TLSCert, s.TLSKey = info.CertFile, info.KeyFile
	}

	// Browser origins: the derived orchestrator origin plus explicit ones.
	explicit := s.AllowOrigins
	if !visited(fs, "allow-origin") && tn.DefaultOrigin != "" {
		explicit = slices.DeleteFunc(slices.Clone(explicit), func(o string) bool { return o == tn.DefaultOrigin })
	}
	explicit = slices.DeleteFunc(slices.Clone(explicit), func(o string) bool { return o == "" })
	tn.DefaultOrigin, tn.OriginSource = "", ""
	if !*t.noDefault {
		origin, source, err := defaultOrchestratorOrigin()
		if err != nil {
			fmt.Fprintf(out, "origins   no default browser origin: %v\n          pass --allow-origin <exact origin of your orchestrator's web app> for browser clients\n", err)
		} else {
			tn.DefaultOrigin, tn.OriginSource = origin, source
		}
	}
	s.AllowOrigins = nil
	for _, o := range append([]string{tn.DefaultOrigin}, explicit...) {
		if o != "" && !slices.Contains(s.AllowOrigins, o) {
			s.AllowOrigins = append(s.AllowOrigins, o)
		}
	}
	if tn.DefaultOrigin != "" {
		fmt.Fprintf(out, "origins   %s (derived from %s)", tn.DefaultOrigin, tn.OriginSource)
		if len(explicit) > 0 {
			fmt.Fprintf(out, " + %s", strings.Join(explicit, ", "))
		}
		fmt.Fprintln(out)
	}

	if err := tailnet.PortProblem(s.Listen, func() bool { return managedOwnsAddr(s.Listen) }); err != nil {
		return err
	}
	s.Tailnet = &tn
	return nil
}

func displayOr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ensureTailnetCert returns a certificate for node in amux's managed store,
// reusing before requesting: the store's current pair if it has more than
// RenewBefore left; else an existing pair in amux's TLS directory (e.g.
// server.crt/server.key) that covers the name with more than RenewBefore
// left, copied into the store with the original untouched; else a fresh one
// from tailscaled.
func ensureTailnetCert(ctx context.Context, c *tailnet.Client, node tailnet.Node, now time.Time) (tailnet.PairInfo, string, error) {
	store := tailnetStore(node.DNSName)
	if _, info, err := store.Load(now); err == nil && info.NotAfter.Sub(now) >= tailnet.RenewBefore {
		return info, "reusing amux's managed certificate for " + node.DNSName, nil
	}
	skipped := ""
	if found, ok := tailnet.FindReusable(tailnetTLSDir(), node.DNSName, tailnet.RenewBefore, now); ok {
		info, err := store.Import(found.CertFile, found.KeyFile, now)
		if err == nil {
			return info, "reusing existing " + found.CertFile + " (copied into amux's store; the original is left as it is)", nil
		}
		// Not usable as a managed (publicly trusted) certificate — e.g. a
		// private-CA pair. It stays where it is; pass it with --tls-cert and
		// --tls-key to serve it as an explicit certificate instead.
		skipped = fmt.Sprintf(" (existing %s not reused: %v)", found.CertFile, err)
	}
	if p := tailnet.CertProblem(node); p != nil {
		p.Msg += skipped
		return tailnet.PairInfo{}, "", p
	}
	certPEM, keyPEM, err := c.CertPair(ctx, node.DNSName, tailnet.RenewBefore)
	if err != nil {
		return tailnet.PairInfo{}, "", tailnet.CertError(err, node)
	}
	info, err := store.Install(certPEM, keyPEM, now)
	if err != nil {
		return tailnet.PairInfo{}, "", err
	}
	return info, "obtained from tailscaled for " + node.DNSName + skipped, nil
}

// defaultOrchestratorOrigin derives the orchestrator web app's origin from
// provider.toml, returning it canonical and where it came from.
func defaultOrchestratorOrigin() (origin, source string, err error) {
	cfg, err := providercfg.Load()
	if errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("no provider is configured (%s)", providercfg.Path())
	}
	if err != nil {
		return "", "", err
	}
	raw, err := tailnet.OrchestratorOrigin(cfg.Orchestrator, cfg.ServerName)
	if err != nil {
		return "", "", err
	}
	c, err := direct.CanonicalOrigin(raw, true)
	if err != nil {
		return "", "", fmt.Errorf("orchestrator %q: %w", cfg.Orchestrator, err)
	}
	return c, "provider.toml orchestrator = " + cfg.Orchestrator, nil
}

// managedOwnsAddr reports whether addr is held by the daemon's own managed
// child — the supervisor runs it, its readiness marker (pid, configuration
// fingerprint) belongs to that child and names addr, and the server answering
// the admin socket is the one the marker names. Only then is a busy port not
// a conflict: re-running enable while the listener runs reconfigures it
// through the supervisor. Nothing here stops, restarts or probes the port.
var managedOwnsAddr = func(addr string) bool {
	st, err := directsvc.ReadStatus(directsvc.StatusPath())
	if err != nil {
		return false
	}
	r := readManagedReady()
	if !st.Serving(r) || r.Addr != addr {
		return false
	}
	srv := directProbeServer()
	return srv != nil && srv.ServerID == r.ServerID
}

// applyTailnetServe completes cfg for a process about to serve in tailnet mode
// (the managed child, or a manual `amux direct serve` from the saved setting
// while the managed listener is off): it re-derives what is not explicit,
// loads the certificate into a reloader, and returns the renewer to run for
// as long as the listener does. A derivation failure is an error, so the
// supervisor retries with backoff rather than binding a stale address.
func applyTailnetServe(ctx context.Context, s directsvc.Settings, cfg *direct.ServeConfig, managed bool, fp string, logf func(string, ...any)) (func(context.Context), error) {
	tn := *s.Tailnet
	now := directTailnetNow()
	client := &tailnet.Client{Socket: tailnet.Socket(tn.Socket)}
	var node tailnet.Node
	if !tn.ListenExplicit || !tn.EndpointExplicit || tn.CertSource == tailnet.CertTailscale {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		n, err := tailnet.Discover(dctx, client)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("tailnet: %w", err)
		}
		node = n
		if n.DNSName != tn.DNSName && tn.DNSName != "" {
			logf("direct: MagicDNS name is now %s (was %s at enable)", n.DNSName, tn.DNSName)
		}
	}
	if !tn.ListenExplicit {
		cfg.Listen = node.Listen(tn.Port)
	}
	if !tn.EndpointExplicit {
		cfg.Endpoints = []string{node.Endpoint(tn.Port)}
	}
	// The certificate must cover the endpoints actually served (after
	// re-derivation), not only the ones checked at enable.
	hosts := tailnet.EndpointHosts(cfg.Endpoints)
	if len(hosts) == 0 {
		return nil, errors.New("tailnet: no endpoint host to serve a certificate for")
	}
	r := &tailnet.Renewer{Mode: tn.CertSource, Logf: logf}
	var info tailnet.PairInfo
	switch tn.CertSource {
	case tailnet.CertExplicit:
		pair, i, err := tailnet.LoadPairFor(s.TLSCert, s.TLSKey, hosts, now)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate for the effective endpoint %s: %w (replace the certificate, or set the endpoint with amux direct enable --endpoint)", strings.Join(cfg.Endpoints, ", "), err)
		}
		r.Target, r.CertFile, r.KeyFile, r.Hosts, info = tailnet.NewReloader(pair), s.TLSCert, s.TLSKey, hosts, i
	default:
		for _, h := range hosts {
			if h != node.DNSName {
				return nil, fmt.Errorf("tailnet: endpoint host %s is not this node's MagicDNS name %s, the only name a tailnet certificate covers; pass --tls-cert and --tls-key (amux direct enable)", h, node.DNSName)
			}
		}
		store := tailnetStore(node.DNSName)
		r.Store = store
		r.Fetch = func(ctx context.Context, min time.Duration) ([]byte, []byte, error) {
			c, k, err := client.CertPair(ctx, node.DNSName, min)
			if err != nil {
				return nil, nil, tailnet.CertError(err, node)
			}
			return c, k, nil
		}
		pair, i, err := store.Load(now)
		if err != nil {
			// The name changed since enable (or the store is gone): obtain one
			// now; the listener cannot start without a certificate.
			fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			c, k, ferr := r.Fetch(fctx, tailnet.RenewBefore)
			cancel()
			if ferr != nil {
				return nil, fmt.Errorf("no certificate for %s: %w", node.DNSName, ferr)
			}
			if _, ierr := store.Install(c, k, now); ierr != nil {
				return nil, ierr
			}
			if pair, i, err = store.Load(now); err != nil {
				return nil, err
			}
		}
		r.Target, info = tailnet.NewReloader(pair), i
	}
	r.Prime(info)
	cfg.GetCertificate = r.Target.GetCertificate
	cfg.CertFile, cfg.KeyFile = "", ""
	var mu sync.Mutex
	drift := ""
	publish := func(cs tailnet.CertState) {
		mu.Lock()
		defer mu.Unlock()
		if err := tailnet.WriteState(tailnetStatePath(), tailnet.State{
			PID: os.Getpid(), Managed: managed, Fingerprint: fp, DNSName: node.DNSName,
			Listen: cfg.Listen, Endpoints: cfg.Endpoints, ControlURL: node.ControlURL, Cert: cs, Drift: drift, UpdatedAt: time.Now(),
		}); err != nil {
			logf("direct: write tailnet state: %v", err)
		}
	}
	r.OnChange = publish
	publish(r.State())
	derived := !tn.ListenExplicit || !tn.EndpointExplicit || tn.CertSource == tailnet.CertTailscale
	watchDrift := func(ctx context.Context) {
		// The address, endpoint and certificate name were derived once, at
		// start. If the tailnet identity changes underneath, say so (status
		// and log) rather than serve a silent mismatch; re-deriving means a
		// restart, which goes through the supervisor (amux direct disable,
		// then enable) or happens at the daemon's next start.
		t := time.NewTicker(directDriftInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			n, err := tailnet.Discover(dctx, client)
			cancel()
			msg := ""
			switch {
			case err != nil:
				msg = "cannot re-read the tailnet identity: " + err.Error()
			case n.DNSName != node.DNSName || (!tn.ListenExplicit && n.IP != node.IP):
				msg = fmt.Sprintf("the tailnet now reports %s (%s) but this listener was derived for %s (%s); turn it off and on (amux direct disable && amux direct enable) to re-derive",
					n.DNSName, n.IP, node.DNSName, node.IP)
			}
			mu.Lock()
			changed := msg != drift
			drift = msg
			mu.Unlock()
			if changed {
				if msg != "" {
					logf("direct: %s", msg)
				}
				publish(r.State())
			}
		}
	}
	return func(ctx context.Context) {
		if derived {
			go watchDrift(ctx)
		}
		r.Run(ctx)
		if st, err := tailnet.ReadState(tailnetStatePath()); err == nil && st.PID == os.Getpid() {
			_ = os.Remove(tailnetStatePath())
		}
	}, nil
}

// settingsServeConfig is the serve configuration a saved setting describes.
func settingsServeConfig(s directsvc.Settings) (direct.ServeConfig, error) {
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

// savedTailnetManualConfig is plain `amux direct serve` (no flags) with
// tailnet mode saved. It never competes with the daemon: while the managed
// listener is enabled the daemon owns this configuration and plain serve
// refuses; while it is off, serve runs it in the foreground under the same
// store lock (which still refuses if any other server owns the store).
func savedTailnetManualConfig() (directsvc.Settings, error) {
	s, set, err := directsvc.Load()
	if err != nil {
		return s, err
	}
	if !set || s.Tailnet == nil {
		return s, errors.New("--listen is required (or save tailnet mode with amux direct enable --tailnet)")
	}
	if s.Enabled {
		return s, errors.New("the daemon-managed listener is enabled and owns the saved configuration; see amux direct status (amux direct disable first to run it in the foreground)")
	}
	return s, nil
}

// directTailnetView is the tailnet part of `amux direct status`.
type directTailnetView struct {
	DNSName       string            `json:"dns_name,omitempty"`
	Port          int               `json:"port"`
	Effective     *tailnet.State    `json:"effective,omitempty"` // from the serving process
	SavedListen   string            `json:"saved_listen"`
	SavedEndpoint []string          `json:"saved_endpoints"`
	Differs       bool              `json:"differs,omitempty"` // effective differs from saved (re-derived)
	CertSource    string            `json:"cert_source"`
	CertNotAfter  time.Time         `json:"cert_not_after,omitempty"`
	CertError     string            `json:"cert_error,omitempty"`
	DefaultOrigin string            `json:"default_origin,omitempty"`
	OriginSource  string            `json:"origin_source,omitempty"`
	Reachability  *directReachProbe `json:"reachability,omitempty"`
}

type directReachProbe struct {
	URL    string `json:"url"`
	OK     bool   `json:"ok"`
	Result string `json:"result"`
}

// tailnetView builds the tailnet status. The effective state counts only when
// it comes from the process serving now: the managed child the supervisor
// runs, or a live manual server.
func tailnetView(s directsvc.Settings, m *directsvc.Status, probe bool) *directTailnetView {
	tn := s.Tailnet
	if tn == nil {
		return nil
	}
	v := &directTailnetView{DNSName: tn.DNSName, Port: tn.Port, SavedListen: s.Listen, SavedEndpoint: s.Endpoints,
		CertSource: tn.CertSource, DefaultOrigin: tn.DefaultOrigin, OriginSource: tn.OriginSource}
	if st, err := tailnet.ReadState(tailnetStatePath()); err == nil && processAlive(st.PID) &&
		(!st.Managed || (m != nil && m.PID == st.PID)) {
		v.Effective = &st
		v.CertNotAfter, v.CertError = st.Cert.NotAfter, st.Cert.LastError
		v.Differs = st.Listen != s.Listen || !slices.Equal(st.Endpoints, s.Endpoints)
	} else if tn.CertSource == tailnet.CertExplicit {
		if _, info, err := tailnet.LoadPairFor(s.TLSCert, s.TLSKey, tailnet.EndpointHosts(s.Endpoints), directTailnetNow()); err == nil {
			v.CertNotAfter = info.NotAfter
		} else {
			v.CertError = err.Error()
		}
	} else if tn.DNSName != "" {
		if _, info, err := tailnetStore(tn.DNSName).Load(directTailnetNow()); err == nil {
			v.CertNotAfter = info.NotAfter
		}
	}
	if probe && v.Effective != nil && len(v.Effective.Endpoints) > 0 {
		v.Reachability = directReachability(v.Effective.Endpoints[0])
	}
	return v
}

// directReachability is a bounded, read-only HTTPS GET of the endpoint from
// this host: it checks that the name resolves, the port answers, and the
// certificate verifies for the name, as a client would see it. A variable so
// tests can replace it.
var directReachability = func(endpoint string) *directReachProbe {
	p := &directReachProbe{URL: endpoint}
	c := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, DisableKeepAlives: true,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(endpoint + "/")
	if err != nil {
		p.Result = err.Error()
		return p
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	p.OK, p.Result = true, fmt.Sprintf("answered HTTP %d with a valid certificate", resp.StatusCode)
	return p
}

func printTailnetView(v *directTailnetView) {
	if v == nil {
		return
	}
	fmt.Printf("tailnet   %s · port %d · cert %s", displayOr(v.DNSName, "(no MagicDNS name)"), v.Port, v.CertSource)
	if !v.CertNotAfter.IsZero() {
		fmt.Printf(" · expires %s (%s)", v.CertNotAfter.Format("2006-01-02"), humanDays(time.Until(v.CertNotAfter)))
	}
	fmt.Println()
	if e := v.Effective; e != nil {
		fmt.Printf("          effective listen %s · endpoints %s\n", e.Listen, strings.Join(e.Endpoints, ", "))
		if v.Differs {
			fmt.Printf("          (re-derived at start; saved %s · %s)\n", v.SavedListen, strings.Join(v.SavedEndpoint, ", "))
		}
		if !e.Cert.NextCheck.IsZero() {
			fmt.Printf("          next certificate check %s\n", e.Cert.NextCheck.Format(time.RFC3339))
		}
	}
	if e := v.Effective; e != nil && e.Drift != "" {
		fmt.Printf("          ⚠ %s\n", e.Drift)
	}
	if v.CertError != "" {
		fmt.Printf("          ⚠ last certificate check failed (still serving the current one): %s\n", v.CertError)
	}
	if v.DefaultOrigin != "" {
		fmt.Printf("          default origin %s (from %s)\n", v.DefaultOrigin, v.OriginSource)
	}
	if p := v.Reachability; p != nil {
		mark := "✗"
		if p.OK {
			mark = "✓"
		}
		fmt.Printf("          %s reach %s: %s\n", mark, p.URL, p.Result)
	}
}

func humanDays(d time.Duration) string {
	if d < 0 {
		return "expired"
	}
	return fmt.Sprintf("%d days left", int(d.Hours()/24))
}

// directClientCount counts active enrolled clients through the running
// server's admin socket (never offline: status must not take the store lock).
var directClientCount = func() (int, bool) {
	cred, err := hostCredential()
	if err != nil {
		return 0, false
	}
	root, err := directRoot()
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := direct.AdminCall(ctx, root, cred, direct.AdminRequest{Op: direct.OpList})
	if err != nil {
		return 0, false
	}
	n := 0
	for _, c := range resp.Clients {
		if c.RevokedAt == 0 {
			n++
		}
	}
	return n, true
}

// reportTailnetDoctor prints doctor's tailnet lines.
func reportTailnetDoctor(v *directTailnetView) {
	endpoint := strings.Join(v.SavedEndpoint, ", ")
	listen := v.SavedListen
	if e := v.Effective; e != nil {
		endpoint, listen = strings.Join(e.Endpoints, ", "), e.Listen
	}
	fmt.Printf("  · tailnet   %s · %s · %s\n", displayOr(v.DNSName, "(no MagicDNS name)"), listen, endpoint)
	switch left := time.Until(v.CertNotAfter); {
	case v.CertNotAfter.IsZero():
		fmt.Printf("  ⚠ cert      %s: expiry unknown\n", v.CertSource)
	case left < 0:
		fmt.Printf("  ✗ cert      %s: expired %s\n", v.CertSource, v.CertNotAfter.Format("2006-01-02"))
	case left < tailnet.RenewBefore && v.CertSource == tailnet.CertExplicit:
		fmt.Printf("  ⚠ cert      explicit: expires %s (%s); amux does not renew explicit certificates\n", v.CertNotAfter.Format("2006-01-02"), humanDays(left))
	default:
		fmt.Printf("  ✓ cert      %s: expires %s (%s)\n", v.CertSource, v.CertNotAfter.Format("2006-01-02"), humanDays(left))
	}
	if v.CertError != "" {
		fmt.Printf("  ⚠ renewal   %s\n", v.CertError)
	}
	if e := v.Effective; e != nil && e.Drift != "" {
		fmt.Printf("  ⚠ drift     %s\n", e.Drift)
	}
	if p := v.Reachability; p != nil {
		mark := "✗"
		if p.OK {
			mark = "✓"
		}
		fmt.Printf("  %s reach     %s: %s\n", mark, p.URL, p.Result)
	}
}
