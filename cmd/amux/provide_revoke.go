package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"amux/internal/provider"
	"amux/internal/providercfg"
	"amux/internal/wiretls"
)

func cmdProvideRevoke(args []string) error {
	if sessionContextRestricted() {
		return errors.New("amux provide revoke is host-only and is not available inside an agent session")
	}
	cfg, err := parseProvideRevoke(args, providercfg.Load)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.Revoke(ctx, cfg); err != nil {
		return err
	}
	fmt.Println("Provider registration and relay access revoked. Other broker replicas close connections on their revocation sweep.")
	fmt.Println("Existing direct-client grants remain valid. Local config, token file and service are retained; use `amux provide uninstall` to remove the service.")
	return nil
}

// parseProvideRevoke validates confirmation before loading any host state.
// Only transport settings are accepted: revocation never advertises capabilities
// or opens a normal provider registration.
func parseProvideRevoke(args []string, load func() (providercfg.Config, error)) (provider.Config, error) {
	fs := flag.NewFlagSet("provide revoke", flag.ContinueOnError)
	var yes bool
	var f provideFlags
	fs.BoolVar(&yes, "yes", false, "revoke this provider's registration and relay access")
	fs.StringVar(&f.orch, "orchestrator", "", "orchestrator address host:port")
	fs.StringVar(&f.tokenFile, "token-file", "", "file holding the provider bearer token")
	fs.StringVar(&f.caFile, "ca", "", "PEM CA file")
	fs.StringVar(&f.serverName, "server-name", "", "TLS server name")
	operands, err := parseFlagsAnyOrder(fs, args)
	if err != nil {
		return provider.Config{}, err
	}
	if !yes {
		return provider.Config{}, errors.New("provide revoke permanently revokes this provider's registration and relay access; rerun with --yes")
	}
	addr, err := provideAddr(f.orch, operands)
	if err != nil {
		return provider.Config{}, err
	}
	file, err := load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return provider.Config{}, fmt.Errorf("provide revoke: %w", err)
	}
	addr = firstNonEmpty(addr, file.Orchestrator)
	if strings.TrimSpace(addr) == "" {
		return provider.Config{}, errors.New("provide revoke: need an orchestrator address")
	}
	token := os.Getenv("AMUX_PROVIDER_TOKEN")
	if path := firstNonEmpty(f.tokenFile, file.TokenFile); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return provider.Config{}, fmt.Errorf("provide revoke: read token file: %w", err)
		}
		token = string(b)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return provider.Config{}, errors.New("provide revoke: need a nonempty provider token (--token-file or AMUX_PROVIDER_TOKEN)")
	}
	return provider.Config{
		Orchestrator: addr, Token: token,
		CAFile:     firstNonEmpty(f.caFile, os.Getenv(wiretls.EnvCA), file.CAFile),
		ServerName: firstNonEmpty(f.serverName, os.Getenv(wiretls.EnvServer), file.ServerName),
	}, nil
}
