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
	"strings"
	"syscall"
	"time"

	"amux/internal/access"
	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/directsvc"
	"amux/internal/hostbackend"
	"amux/internal/provider"
)

// cmdDirect is `amux direct`: opt-in direct client access over an existing LAN
// or VPN, with no cloud in the data path. See docs/direct-access.md.
func cmdDirect(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "" || isHelpFlag(sub) {
		directUsage()
		return nil
	}
	run, ok := directSubcommands[sub]
	if !ok {
		directUsage()
		return fmt.Errorf("unknown direct command %q", sub)
	}
	// The direct server and its administration act with host authority. A
	// restricted agent session must never start or administer it, whatever
	// credential files it can name.
	if sessionContextRestricted() {
		return errors.New("amux direct is host-only and is not available inside an agent session")
	}
	return run(args[1:])
}

var directSubcommands = map[string]func([]string) error{
	"serve":          cmdDirectServe,
	"enroll":         cmdDirectEnroll,
	"clients":        cmdDirectClients,
	"reset-identity": cmdDirectResetIdentity,
	"cloud":          cmdDirectCloud,
	"enable":         cmdDirectEnable,
	"disable":        cmdDirectDisable,
	"status":         cmdDirectStatus,
	"logs":           cmdDirectLogs,
}

func directUsage() {
	w := os.Stderr
	fmt.Fprint(w, `amux direct — direct client access (web, Android) over your LAN or VPN

The direct server is an opt-in HTTPS listener on a private address. Clients
enroll with a one-time code you paste, then reach this host's sessions with
destination-issued credentials. No cloud service is in the path.

usage: amux direct <command>

  enable [serve flags]        save the listener settings and turn it on; the
                              daemon runs it from now on (starting the daemon
                              if it is stopped). Off by default.
  disable                     turn the daemon-managed listener off (settings,
                              enrolled clients and cloud policy are kept)
  status [--json]             setting, managed listener, server, provider offer
  logs [-n lines]             the managed listener's log
  serve                       run the direct server in the foreground (manual)
  enroll                      print a one-time enrollment bundle to paste into a client
  clients list                list enrolled clients
  clients revoke <client-id>  revoke a client now (closes its streams first)
  reset-identity --yes        replace the server identity; revokes every client
  cloud enable|disable|status  cloud-assisted enrollment policy (off by default)

serve flags (also accepted by enable, which saves them; enable merges what
you type over the saved settings, and --no-wait skips waiting for readiness):
  --listen <ip:port>      address to bind (a private or loopback IP literal)
  --endpoint <origin>     https origin clients dial, e.g. https://amux.home.example:8443 (repeatable)
  --allow-origin <origin> exact browser origin allowed to call (repeatable)
  --tls-cert <pem>        certificate chain (browser use needs a browser-trusted one)
  --tls-key <pem>         private key
  --allow-public-bind     permit a non-private listen address (deliberate exposure)
  --access-ttl <dur>      access token lifetime (default and maximum 10m)
  --idle-ttl <dur>        refresh idle lifetime (default and maximum 720h)
  --absolute-ttl <dur>    grant absolute lifetime (default and maximum 2160h)

enroll flags (the running server publishes its endpoints into the bundle):
  --label <text>          name for the client, shown in clients list
  --permissions <list>    read | read,steer   (required)
  --all-sessions          grant every session on this host, or
  --workgroup <root-id>   grant one workgroup and its members (repeatable)
  --ttl <dur>             code lifetime (default 10m, maximum 1h)
  --json                  print the full response as JSON

cloud enable flags (the ceiling is required; all-sessions covers ordinary
sessions, never the amux console):
  --permissions <list>    ceiling: read | read,steer
  --all-sessions          ceiling: any ordinary session, or
  --workgroup <root-id>   ceiling: these workgroups (repeatable)
  --owner <user-id>       advanced: the Harness user id allowed to request codes
  --provider <id>         advanced: this host's Harness provider id
                          Both default to the owner and provider Harness
                          reported at this host's latest amux provide
                          registration (shown by amux doctor). A flag that
                          disagrees with it is refused unless both are given.
  --max-outstanding <n>   unredeemed cloud codes at once (default 3)
  --max-per-hour <n>      cloud codes per hour (default 10)

Disabling the managed listener is not revocation: clients stay enrolled and
the cloud policy stays as it is, ready for the next enable. Use
amux direct clients revoke and amux direct cloud disable for those.

Enabling cloud enrollment trusts the pinned Harness owner, through Harness, to
mint enrollment codes up to the ceiling. Codes are bound to the requesting
client's PKCE verifier and last 5 minutes. Every enable, disable or re-pin
invalidates outstanding cloud codes; grants already redeemed stay until revoked
(amux direct clients revoke). Identity reset disables cloud enrollment.
`)
}

func directRoot() (string, error) { return direct.DefaultRoot() }

// hostCredential loads the host credential from the protected directory, which
// HOME and XDG cannot redirect.
func hostCredential() (access.Credential, error) {
	dir, err := access.ProtectedHostCredentialDir()
	if err != nil {
		return access.Credential{}, err
	}
	cred, err := access.LoadCredential(dir)
	if err != nil {
		return access.Credential{}, fmt.Errorf("load the host credential (start the daemon with `amux daemon start`): %w", err)
	}
	if cred.Kind != access.SubjectHost || cred.SubjectID != access.LocalHostSubject {
		return access.Credential{}, access.ErrInvalidCredential
	}
	return cred, nil
}

func relayHostProof(ctx context.Context, prove direct.ProveFunc) error {
	return daemon.RelayHostProof(ctx, prove)
}

func cmdDirectServe(args []string) error {
	fs := flag.NewFlagSet("amux direct serve", flag.ContinueOnError)
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
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var cfg direct.ServeConfig
	if *managed {
		// The daemon's child: the whole configuration is the saved setting, so
		// the command line stays constant and nothing typed can widen it.
		typed := 0
		fs.Visit(func(*flag.Flag) { typed++ })
		if typed != 1 {
			return errors.New("--managed takes its configuration from the saved setting (amux direct enable), not flags")
		}
		s, set, err := directsvc.Load()
		if err != nil {
			return err
		}
		if cfg, err = managedServeConfig(s, set); err != nil {
			return err
		}
		// A daemon that died without stopping us must not leave the listener
		// holding the direct store (Linux also delivers a parent-death signal).
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go directsvc.WatchParent(2*time.Second, ctx.Done(), func() {
			fmt.Fprintln(os.Stderr, "amux direct: supervising daemon exited; stopping")
			cancel()
		})
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
	cfg.Logf = func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) }
	cfg.Ready = func(addr net.Addr, serverID string) {
		fmt.Fprintf(os.Stderr, "amux direct: listening on %s (server_id %s)\n", addr, serverID)
	}
	err = direct.Serve(ctx, cfg)
	if err != nil && !*managed && strings.Contains(err.Error(), "already owns the direct store") {
		err = fmt.Errorf("%w (if the daemon manages it, see amux direct status; amux direct disable turns it off)", err)
	}
	return err
}

// adminRun sends req to the running server, or, when none owns the store,
// runs it offline under the store's exclusive lock. Both require host proof.
func adminRun(req direct.AdminRequest) (direct.AdminResponse, error) {
	cred, err := hostCredential()
	if err != nil {
		return direct.AdminResponse{}, err
	}
	root, err := directRoot()
	if err != nil {
		return direct.AdminResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := direct.AdminCall(ctx, root, cred, req)
	if err == nil {
		return resp, nil
	}
	var nerr *net.OpError
	if !errors.As(err, &nerr) && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return resp, err
	}
	// No server is listening: administer the store directly. If a server owns
	// the lock after all, this refuses rather than writing beside it.
	resp, err = direct.OfflineAdminister(ctx, root, cred, relayHostProof, req)
	if errors.Is(err, direct.ErrStoreBusy) {
		return resp, errors.New("the direct store is owned by a running server whose admin socket is unreachable")
	}
	return resp, err
}

func cmdDirectEnroll(args []string) error {
	fs := flag.NewFlagSet("amux direct enroll", flag.ContinueOnError)
	var groups multiFlag
	label := fs.String("label", "", "")
	perms := fs.String("permissions", "", "")
	all := fs.Bool("all-sessions", false, "")
	ttl := fs.Duration("ttl", 0, "")
	asJSON := fs.Bool("json", false, "")
	fs.Var(&groups, "workgroup", "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if *label == "" || *perms == "" {
		return errors.New("--label and --permissions are required")
	}
	resource, err := directResourceFlags(*all, groups)
	if err != nil {
		return err
	}
	resp, err := adminRun(direct.AdminRequest{
		Op: direct.OpEnroll, Label: *label, Permissions: strings.Split(*perms, ","), Resource: resource,
		TTLSeconds: int64(*ttl / time.Second),
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	fmt.Fprintf(os.Stderr, "Paste this into the client within %s (single use; server_id %s):\n\n",
		time.Until(time.Unix(resp.ExpiresAt, 0)).Round(time.Second), resp.ServerID)
	fmt.Println(resp.Bundle)
	return nil
}

func cmdDirectClients(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: amux direct clients list | revoke <client-id>")
	}
	switch args[0] {
	case "list", "ls":
		asJSON := len(args) > 1 && args[1] == "--json"
		resp, err := adminRun(direct.AdminRequest{Op: direct.OpList})
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(resp.Clients)
		}
		fmt.Printf("server_id %s\n", resp.ServerID)
		for _, c := range resp.Clients {
			state := "active"
			if c.RevokedAt != 0 {
				state = "revoked (" + c.RevokeReason + ")"
			}
			if c.Origin == direct.OriginCloud {
				state += "  cloud:" + c.CloudRequester
			}
			scope := c.ResourceScope.Kind
			if len(c.ResourceScope.Workgroups) > 0 {
				scope += ":" + strings.Join(c.ResourceScope.Workgroups, ",")
			}
			fmt.Printf("%s  %-20s %-8s %-10s %-24s %s\n", c.ClientID, c.Label, c.ClientKind,
				strings.Join(c.Permissions, ","), scope, state)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: amux direct clients revoke <client-id>")
		}
		if _, err := adminRun(direct.AdminRequest{Op: direct.OpRevoke, ClientID: args[1]}); err != nil {
			return err
		}
		fmt.Printf("revoked %s\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown clients command %q", args[0])
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
		return errors.New("usage: amux direct cloud enable|disable|status")
	}
	switch args[0] {
	case "enable":
		return cmdDirectCloudEnable(args[1:])
	case "disable":
		if len(args) != 1 {
			return errors.New("usage: amux direct cloud disable")
		}
		if _, err := adminRun(direct.AdminRequest{Op: direct.OpCloudDisable}); err != nil {
			return err
		}
		fmt.Println("cloud enrollment disabled; outstanding cloud codes are void. Grants already redeemed stay until `amux direct clients revoke`.")
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
	fs := flag.NewFlagSet("amux direct cloud enable", flag.ContinueOnError)
	var groups multiFlag
	owner := fs.String("owner", "", "")
	prov := fs.String("provider", "", "")
	perms := fs.String("permissions", "", "")
	all := fs.Bool("all-sessions", false, "")
	maxOut := fs.Int("max-outstanding", 0, "")
	perHour := fs.Int("max-per-hour", 0, "")
	fs.Var(&groups, "workgroup", "")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *perms == "" {
		return errors.New("--permissions is required (read or read,steer)")
	}
	resource, err := directResourceFlags(*all, groups)
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
		Permissions: strings.Split(*perms, ","), Resource: resource, OwnerPin: pins.Owner, ProviderPin: pins.Provider,
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
	return direct.ResourceScope{}, errors.New("choose the resource scope explicitly: --all-sessions or --workgroup <root-id>")
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
