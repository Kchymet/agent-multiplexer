package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"amux/internal/provider"
	"amux/internal/wiretls"
)

// cmdProvide runs provider mode: dial out to a remote orchestrator over TLS,
// register this machine as a compute node, and serve harnessproto v2
// (spawn/input/resize/kill ⇄ output/exit) over the connection. Panes survive
// reconnects within the orchestrator's grace window. See docs/remote-provider.md.
//
//	amux provide <orchestrator-addr> [flags]
//	amux provide --orchestrator host:port [flags]
//
// Config resolves from flags, then the AMUX_PROVIDER_* / AMUX_TLS_* env vars.
// The token is never taken from argv (a bearer credential): use --token-file or
// AMUX_PROVIDER_TOKEN.
func cmdProvide(args []string) error {
	fs := flag.NewFlagSet("provide", flag.ContinueOnError)
	var (
		orch       = fs.String("orchestrator", "", "orchestrator address host:port (or as the positional arg)")
		tokenFile  = fs.String("token-file", "", "file holding the bearer token (mode 0600); preferred over $AMUX_PROVIDER_TOKEN")
		name       = fs.String("name", "", "provider display name (default $AMUX_PROVIDER_NAME or hostname)")
		caFile     = fs.String("ca", "", "PEM CA file to trust in addition to the system roots (default $AMUX_TLS_CA)")
		serverName = fs.String("server-name", "", "TLS server name for SNI/verification (default $AMUX_TLS_SERVERNAME)")
		maxPanes   = fs.Int("max-panes", 0, "capability: max concurrent panes (default $AMUX_PROVIDER_MAX_PANES)")
		labels     multiFlag
		features   multiFlag
	)
	fs.Var(&labels, "label", "scheduling label key=value (repeatable); merged over $AMUX_PROVIDER_LABELS")
	fs.Var(&features, "feature", "capability feature string (repeatable); merged with $AMUX_PROVIDER_FEATURES")
	if err := fs.Parse(args); err != nil {
		return err
	}

	addr := *orch
	if addr == "" && fs.NArg() > 0 {
		addr = fs.Arg(0)
	}
	if addr == "" {
		return fmt.Errorf("provide: need an orchestrator address (positional or --orchestrator)")
	}

	token := os.Getenv("AMUX_PROVIDER_TOKEN")
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("provide: read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}

	displayName := *name
	if displayName == "" {
		displayName = os.Getenv("AMUX_PROVIDER_NAME")
	}
	if displayName == "" {
		displayName, _ = os.Hostname()
	}

	ca := *caFile
	if ca == "" {
		ca = os.Getenv(wiretls.EnvCA)
	}
	sni := *serverName
	if sni == "" {
		sni = os.Getenv(wiretls.EnvServer)
	}

	mp := *maxPanes
	if mp == 0 {
		if s := os.Getenv("AMUX_PROVIDER_MAX_PANES"); s != "" {
			mp, _ = strconv.Atoi(s)
		}
	}

	cfg := provider.Config{
		Orchestrator: addr,
		Token:        token,
		Name:         displayName,
		Labels:       parseLabels(os.Getenv("AMUX_PROVIDER_LABELS"), labels),
		CAFile:       ca,
		ServerName:   sni,
		MaxPanes:     mp,
		Features:     mergeFeatures(os.Getenv("AMUX_PROVIDER_FEATURES"), features),
		Logf:         func(format string, a ...any) { fmt.Fprintf(os.Stderr, "amux provide: "+format+"\n", a...) },
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.New(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// multiFlag collects a repeatable string flag (--label, --feature).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseLabels merges comma-separated key=value pairs from env with repeated
// --label flags (flags win on conflict).
func parseLabels(env string, flags multiFlag) map[string]string {
	out := map[string]string{}
	add := func(pair string) {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			return
		}
		k, v, _ := strings.Cut(pair, "=")
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if env != "" {
		for _, p := range strings.Split(env, ",") {
			add(p)
		}
	}
	for _, p := range flags {
		add(p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mergeFeatures combines comma-separated env features with repeated --feature
// flags into an ordered, de-duplicated list. Feature strings are opaque.
func mergeFeatures(env string, flags multiFlag) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	if env != "" {
		for _, f := range strings.Split(env, ",") {
			add(f)
		}
	}
	for _, f := range flags {
		add(f)
	}
	return out
}
