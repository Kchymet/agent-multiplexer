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
	"amux/internal/hostbackend"
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
}

func directUsage() {
	w := os.Stderr
	fmt.Fprint(w, `amux direct — direct client access (web, Android) over your LAN or VPN

The direct server is an opt-in HTTPS listener on a private address. Clients
enroll with a one-time code you paste, then reach this host's sessions with
destination-issued credentials. No cloud service is in the path.

usage: amux direct <command>

  serve                       run the direct server in the foreground
  enroll                      print a one-time enrollment bundle to paste into a client
  clients list                list enrolled clients
  clients revoke <client-id>  revoke a client now (closes its streams first)
  reset-identity --yes        replace the server identity; revokes every client

serve flags:
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
`)
}

func directRoot() string { return direct.DefaultRoot() }

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
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		directUsage()
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *listen == "" {
		return errors.New("--listen is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return direct.Serve(ctx, direct.ServeConfig{
		Root: directRoot(), Listen: *listen, AllowPublicBind: *public, CertFile: *cert, KeyFile: *key,
		Endpoints: endpoints, AllowOrigins: origins,
		Life:    direct.Lifetimes{Access: *accessTTL, Idle: *idle, Absolute: *absolute},
		Backend: direct.HostBackend{H: hostbackend.New()},
		Verify:  relayHostProof,
		Logf:    func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) },
		Ready: func(addr net.Addr, serverID string) {
			fmt.Fprintf(os.Stderr, "amux direct: listening on %s (server_id %s)\n", addr, serverID)
		},
	})
}

// adminRun sends req to the running server, or, when none owns the store,
// runs it offline under the store's exclusive lock. Both require host proof.
func adminRun(req direct.AdminRequest) (direct.AdminResponse, error) {
	cred, err := hostCredential()
	if err != nil {
		return direct.AdminResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := direct.AdminCall(ctx, directRoot(), cred, req)
	if err == nil {
		return resp, nil
	}
	var nerr *net.OpError
	if !errors.As(err, &nerr) && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return resp, err
	}
	// No server is listening: administer the store directly. If a server owns
	// the lock after all, this refuses rather than writing beside it.
	resp, err = direct.OfflineAdminister(ctx, directRoot(), cred, relayHostProof, req)
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
	var resource direct.ResourceScope
	switch {
	case *all && len(groups) > 0:
		return errors.New("choose --all-sessions or --workgroup, not both")
	case *all:
		resource = direct.ResourceScope{Kind: direct.ResourceAllSessions}
	case len(groups) > 0:
		resource = direct.ResourceScope{Kind: direct.ResourceWorkgroups, Workgroups: groups}
	default:
		return errors.New("choose the resource scope explicitly: --all-sessions or --workgroup <root-id>")
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
