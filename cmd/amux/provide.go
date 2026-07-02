package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/engine"
	"amux/internal/provider"
	"amux/internal/source"
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
		publishSes = fs.Bool("publish-sessions", false, "advertise the sessions feature: publish this daemon's session inventory and accept lifecycle verbs (default $AMUX_PROVIDER_PUBLISH_SESSIONS)")
		readOnly   = fs.Bool("read-only-sessions", false, "publish inventory but reject every lifecycle verb (default $AMUX_PROVIDER_SESSIONS_READONLY)")
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

	publish := *publishSes || envBool("AMUX_PROVIDER_PUBLISH_SESSIONS")
	readonly := *readOnly || envBool("AMUX_PROVIDER_SESSIONS_READONLY")

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
	if publish {
		// The session rail is the daemon's own inventory: a store-backed poll,
		// annotated with engine liveness (which lights up AAP-derived state) read
		// from the file the running daemon persists — so publishing needs no second
		// connection to the daemon. Lifecycle verbs, in contrast, run through the
		// daemon socket so the daemon stays authoritative (it owns the engine and the
		// re-poll that surfaces the change); with no daemon reachable they fail cleanly.
		ws := source.NewWorkspace()
		ws.SetLiveness(persistedLiveAgents)
		cfg.PublishSessions = true
		cfg.ReadOnlySessions = readonly
		cfg.Sessions = ws.Poll
		if !readonly {
			cfg.ApplyAction = applyViaDaemon
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.New(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// envBool reports whether an env var is set to a truthy value (1/true/yes/on).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// persistedLiveAgents reads the set of agent ids the running daemon last recorded
// as live in its engine (the agent tab), so the published inventory can light up
// AAP-derived state without a second connection to the daemon. A missing or
// unreadable file yields an empty set (everything reads idle), never an error.
func persistedLiveAgents() map[string]bool {
	out := map[string]bool{}
	buf, err := os.ReadFile(core.LiveAgentsPath())
	if err != nil {
		return out
	}
	var keys []engine.Key
	if err := json.Unmarshal(buf, &keys); err != nil {
		return out
	}
	for _, k := range keys {
		if k.Tab == 0 { // TabAgent: the agent process, not editor/terminal
			out[k.AgentID] = true
		}
	}
	return out
}

// applyViaDaemon runs one lifecycle verb through the local daemon so the daemon
// stays authoritative — it owns the engine (needed for "start") and re-polls to
// surface the change. It dials fresh per call (verbs are infrequent), sends the
// action, and returns the id of any session it created. Snapshot frames that
// arrive first are skipped; a non-OK result surfaces the daemon's error.
func applyViaDaemon(ctx context.Context, a core.Action) (string, error) {
	c, err := daemon.Dial()
	if err != nil {
		return "", fmt.Errorf("daemon unreachable: %w", err)
	}
	defer c.Close()
	if err := c.Send(a); err != nil {
		return "", err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return "", err
		}
		if f.Result != nil {
			if !f.Result.OK {
				return "", errors.New(f.Result.Error)
			}
			return f.Result.NewID, nil
		}
	}
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
