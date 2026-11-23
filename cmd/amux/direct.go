package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"amux/internal/access"
	"amux/internal/direct"
	"amux/internal/directadmin"
	"amux/internal/directsvc"
	"amux/internal/hostbackend"
	"amux/internal/provider"
)

// cmdDirect is `amux device`: opt-in direct client access over an existing LAN
// or VPN, with no cloud in the data path. See docs/direct-access.md.
func directRoot() (string, error) { return direct.DefaultRoot() }

// hostCredential loads the host credential from the protected directory, which
// HOME and XDG cannot redirect.
func hostCredential() (access.Credential, error) { return directadmin.HostCredential() }

func relayHostProof(ctx context.Context, prove direct.ProveFunc) error {
	return directadmin.RelayHostProof(ctx, prove)
}

func cmdDirectServe(args []string) error {
	fs := newCmdFlagSet("device serve")
	var endpoints, origins multiFlag
	listen := fs.String("listen", "", "")
	cert := fs.String("tls-cert", os.Getenv("AMUX_TLS_CERT"), "")
	key := fs.String("tls-key", os.Getenv("AMUX_TLS_KEY"), "")
	public := fs.Bool("allow-public-bind", false, "")
	accessTTL := fs.Duration("access-ttl", 0, "")
	idle := fs.Duration("idle-ttl", 0, "")
	absolute := fs.Duration("absolute-ttl", 0, "")
	fs.Var(&endpoints, "endpoint", "")
	fs.Var(&origins, "allow-origin", "")
	managed := fs.Bool("managed", false, "")
	if err := parseCmdFlags(fs, "device serve", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var cfg direct.ServeConfig
	var managedReady func(net.Addr, string)
	var tailnetRun func(context.Context)
	logf := func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	if *managed {
		// The daemon's child: the whole configuration is the saved setting, so
		// the command line stays constant and nothing typed can widen it.
		typed := 0
		fs.Visit(func(*flag.Flag) { typed++ })
		if typed != 1 {
			return errors.New("--managed takes its configuration from the saved setting (amux device enable), not flags")
		}
		// Ownership first: the daemon named itself before exec, so a daemon
		// that died before this point is caught here, before anything binds.
		supervisor, err := directsvc.SupervisorPID(os.Getenv)
		if err != nil {
			return err
		}
		if err := directsvc.CheckParent(supervisor); err != nil {
			return fmt.Errorf("amux device: %w; not starting", err)
		}
		s, set, err := directsvc.Load()
		if err != nil {
			return err
		}
		if cfg, err = managedServeConfig(s, set); err != nil {
			return err
		}
		if s.Tailnet != nil {
			// Re-derive the address and endpoint from tailscaled and load the
			// certificate this child will renew and reload in place. The
			// fingerprint stays the saved configuration's, so readiness still
			// correlates with what the supervisor started.
			if tailnetRun, err = applyTailnetServe(ctx, s, &cfg, true, directsvc.Fingerprint(s), logf); err != nil {
				return err
			}
		}
		// A daemon that dies later must not leave the listener holding the
		// direct store either (Linux also delivers a parent-death signal).
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go directsvc.WatchParent(supervisor, 2*time.Second, ctx.Done(), func() {
			fmt.Fprintln(os.Stderr, "amux device: supervising daemon exited; stopping")
			cancel()
		})
		fp := directsvc.Fingerprint(s)
		managedReady = func(addr net.Addr, serverID string) {
			// The marker is what lets enable/status tell this child's server
			// from a manual one answering the same admin socket.
			if err := directsvc.WriteReady(directsvc.ReadyPath(), directsvc.Ready{
				PID: os.Getpid(), ServerID: serverID, Addr: addr.String(), Fingerprint: fp, At: time.Now(),
			}); err != nil {
				fmt.Fprintf(os.Stderr, "amux device: write readiness marker: %v\n", err)
			}
		}
	} else if typed := visited(fs, "listen", "tls-cert", "tls-key", "allow-public-bind", "access-ttl", "idle-ttl", "absolute-ttl", "endpoint", "allow-origin"); !typed && *listen == "" {
		// Plain `amux device serve`: run the saved tailnet configuration in
		// the foreground, but only while the daemon is not managing it.
		s, err := savedTailnetManualConfig()
		if err != nil {
			return err
		}
		if cfg, err = settingsServeConfig(s); err != nil {
			return err
		}
		if tailnetRun, err = applyTailnetServe(ctx, s, &cfg, false, "", logf); err != nil {
			return err
		}
	} else {
		if *listen == "" {
			return errors.New("--listen is required")
		}
		cfg = direct.ServeConfig{
			Listen: *listen, AllowPublicBind: *public, CertFile: *cert, KeyFile: *key,
			Endpoints: endpoints, AllowOrigins: origins,
			Life: direct.Lifetimes{Access: *accessTTL, Idle: *idle, Absolute: *absolute},
		}
	}
	root, err := directRoot()
	if err != nil {
		return err
	}
	cfg.Root = root
	cfg.Backend = direct.HostBackend{H: hostbackend.New()}
	cfg.Verify = relayHostProof
	cfg.Logf = logf
	if tailnetRun != nil {
		// Renewal and drift checks live exactly as long as this listener.
		rctx, rcancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { tailnetRun(rctx); close(done) }()
		defer func() { rcancel(); <-done }()
	}
	cfg.Ready = func(addr net.Addr, serverID string) {
		fmt.Fprintf(os.Stderr, "amux device: listening on %s (server_id %s)\n", addr, serverID)
		if managedReady != nil {
			managedReady(addr, serverID)
		}
	}
	err = direct.Serve(ctx, cfg)
	if err != nil && !*managed && strings.Contains(err.Error(), "already owns the direct store") {
		err = fmt.Errorf("%w (if the daemon manages it, see amux device status; amux device disable turns it off)", err)
	}
	return err
}

// adminRun sends req to the running server, or, when none owns the store,
// runs it offline under the store's exclusive lock. Both require host proof.
func adminRun(req direct.AdminRequest) (direct.AdminResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return directadmin.Run(ctx, req)
}

func cmdDirectClients(args []string) error {
	return runDirectClients(args, os.Stdout, adminRun)
}

func runDirectClients(args []string, out io.Writer, admin func(direct.AdminRequest) (direct.AdminResponse, error)) error {
	if len(args) == 0 {
		return errors.New("usage: amux device ls [--stale [--idle-days N]] [--json] | update <client-id> ... | revoke <client-id>")
	}
	switch args[0] {
	case "list", "ls":
		fs := newCmdFlagSet("device ls")
		asJSON := fs.Bool("json", false, "")
		stale := fs.Bool("stale", false, "")
		idle := fs.Int("idle-days", 0, "")
		if err := parseCmdFlags(fs, "device ls", args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		if *idle < 0 || (*idle != 0 && !*stale) {
			return errors.New("--idle-days N (positive) applies to --stale")
		}
		resp, err := admin(direct.AdminRequest{Op: direct.OpList, Stale: *stale, StaleDays: *idle})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(out).Encode(resp.Clients)
		}
		printDirectClients(out, resp.ServerID, resp.Clients, *stale)
		return nil
	case "update":
		return runDirectClientsUpdate(args[1:], out, admin)
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: amux device revoke <client-id>")
		}
		if _, err := admin(direct.AdminRequest{Op: direct.OpRevoke, ClientID: args[1]}); err != nil {
			return err
		}
		fmt.Fprintf(out, "revoked %s\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown clients command %q", args[0])
}

// cmdDirectAudit prints the direct audit log (internal/direct/audit.go).
func cmdDirectAudit(args []string) error {
	req := direct.AdminRequest{Op: direct.OpAudit}
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			asJSON = true
		case "--client":
			if i+1 >= len(args) {
				return errors.New("--client needs a client id")
			}
			i++
			req.ClientID = args[i]
		case "--limit":
			if i+1 >= len(args) {
				return errors.New("--limit needs a number")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				return errors.New("--limit must be a positive number")
			}
			req.Limit = n
		default:
			return usagef("unknown audit argument %q", args[i])
		}
	}
	resp, err := adminRun(req)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp.Audit)
	}
	if len(resp.Audit) == 0 {
		fmt.Println("(no audited direct actions)")
		return nil
	}
	for _, r := range resp.Audit {
		who := r.ClientID
		if r.Label != "" {
			who += " (" + r.Label + ")"
		}
		text := strings.Join(strings.Fields(r.Text), " ")
		if len(text) > 80 {
			text = text[:80] + "…"
		}
		fmt.Printf("%s  %s  %-15s %-16s %-14s %-12s %s\n", time.UnixMilli(r.At).Format(time.RFC3339), who, r.Route, r.Verb, r.Target, r.Outcome, text)
	}
	return nil
}

func cmdDirectResetIdentity(args []string) error {
	if len(args) != 1 || args[0] != "--yes" {
		return errors.New("reset-identity revokes every enrolled client; rerun with --yes")
	}
	resp, err := adminRun(direct.AdminRequest{Op: direct.OpResetIdentity})
	if err != nil {
		return err
	}
	fmt.Printf("new server_id %s; every client must enroll again\n", resp.ServerID)
	return nil
}

func cmdDirectCloud(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: amux cloud enroll enable|disable|status")
	}
	switch args[0] {
	case "enable":
		return cmdDirectCloudEnable(args[1:])
	case "disable":
		if len(args) != 1 {
			return errors.New("usage: amux cloud enroll disable")
		}
		if _, err := adminRun(direct.AdminRequest{Op: direct.OpCloudDisable}); err != nil {
			return err
		}
		fmt.Println("cloud enrollment disabled; outstanding cloud codes are void. Grants already redeemed stay until `amux device revoke`.")
		return nil
	case "status":
		asJSON := len(args) > 1 && args[1] == "--json"
		resp, err := adminRun(direct.AdminRequest{Op: direct.OpCloudStatus})
		if err != nil {
			return err
		}
		if resp.Cloud == nil {
			resp.Cloud = &direct.CloudStatus{}
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(resp.Cloud)
		}
		printCloudStatus(*resp.Cloud)
		st, stErr := provider.ReadStatus(provider.StatusPath())
		if note := cloudPinDrift(*resp.Cloud, st, stErr); note != "" {
			fmt.Println(note)
		}
		return nil
	}
	return fmt.Errorf("unknown cloud command %q", args[0])
}

func cmdDirectCloudEnable(args []string) error {
	fs := newCmdFlagSet("cloud enroll enable")
	var groups multiFlag
	owner := fs.String("owner", "", "")
	prov := fs.String("provider", "", "")
	perms := fs.String("permissions", "", "")
	all := fs.Bool("all-sessions", false, "")
	maxOut := fs.Int("max-outstanding", 0, "")
	perHour := fs.Int("max-per-hour", 0, "")
	fs.Var(&groups, "workgroup", "")
	if err := parseCmdFlags(fs, "cloud enroll enable", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// The default ceiling is read, steer and the execute set over every
	// ordinary session (never the console); manage, create and accounts are
	// opt-in.
	permList := append([]string(nil), direct.DefaultPermissions...)
	if *perms != "" {
		permList = direct.ExpandPermAliases(strings.Split(*perms, ","))
	}
	resource, err := directResourceFlags(*all, groups) // defaults to all ordinary sessions
	if err != nil {
		return err
	}
	st, stErr := provider.ReadStatus(provider.StatusPath())
	pins, err := resolveCloudPins(*owner, *prov, st, stErr, processAlive)
	if err != nil {
		return err
	}
	fmt.Println(pins.describe())
	if pins.Note != "" {
		fmt.Println(pins.Note)
	}
	resp, err := adminRun(direct.AdminRequest{Op: direct.OpCloudEnable, Cloud: &direct.CloudPolicyRequest{
		Permissions: permList, Resource: resource, OwnerPin: pins.Owner, ProviderPin: pins.Provider,
		MaxOutstanding: *maxOut, MaxPerHour: *perHour,
	}})
	if err != nil {
		return err
	}
	if resp.Cloud != nil {
		printCloudStatus(*resp.Cloud)
	}
	return nil
}

func directResourceFlags(all bool, groups []string) (direct.ResourceScope, error) {
	switch {
	case all && len(groups) > 0:
		return direct.ResourceScope{}, errors.New("choose --all-sessions or --workgroup, not both")
	case all:
		return direct.ResourceScope{Kind: direct.ResourceAllSessions}, nil
	case len(groups) > 0:
		return direct.ResourceScope{Kind: direct.ResourceWorkgroups, Workgroups: groups}, nil
	}
	// The default scope is every ordinary session. The console is not a
	// session any scope covers (ValidateResource refuses it by id, and
	// all_sessions excludes it), so the default never reaches it.
	return direct.ResourceScope{Kind: direct.ResourceAllSessions}, nil
}

func printCloudStatus(c direct.CloudStatus) {
	if !c.Enabled {
		fmt.Printf("cloud enrollment: disabled (generation %d", c.Generation)
		if c.DisableReason != "" {
			fmt.Printf(", %s", c.DisableReason)
		}
		fmt.Println(")")
		return
	}
	scope := c.Resource.Kind
	if len(c.Resource.Workgroups) > 0 {
		scope += ":" + strings.Join(c.Resource.Workgroups, ",")
	}
	fmt.Printf("cloud enrollment: enabled (generation %d)\n", c.Generation)
	fmt.Printf("  owner %s  provider %s\n", c.OwnerPin, c.ProviderPin)
	fmt.Printf("  ceiling %s %s\n", strings.Join(c.Permissions, ","), scope)
	fmt.Printf("  outstanding %d/%d  issued last hour %d/%d\n", c.Outstanding, c.MaxOutstanding, c.IssuedLastHour, c.MaxPerHour)
}
