package main

import (
	"context"
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
	"amux/internal/provider"
	"amux/internal/runtimeevents"
	"amux/internal/wiretls"
)

// cmdProvide runs provider mode: dial out to a remote orchestrator over TLS,
// register this machine as a compute node, and serve harnessproto v2
// (spawn/input/resize/kill ⇄ output/exit) over the connection. Panes survive
// reconnects within the orchestrator's grace window. See docs/remote-provider.md.
//
//	amux provide <orchestrator-addr> [flags]
//	amux provide [flags] <orchestrator-addr>
//	amux provide --orchestrator host:port [flags]
//
// Flags work on either side of the address (parseFlagsAnyOrder) — the first
// phrasing above is the one people reach for, and it used to drop every flag
// after the address without a word.
//
// Config resolves from flags, then the AMUX_PROVIDER_* / AMUX_TLS_* env vars.
// The token is never taken from argv (a bearer credential): use --token-file or
// AMUX_PROVIDER_TOKEN.
func cmdProvide(args []string) error {
	fs := flag.NewFlagSet("provide", flag.ContinueOnError)
	var f provideFlags
	f.register(fs)
	operands, err := parseFlagsAnyOrder(fs, args)
	if err != nil {
		return err
	}

	addr, err := provideAddr(f.orch, operands)
	if err != nil {
		return err
	}
	if addr == "" {
		return fmt.Errorf("provide: need an orchestrator address (positional or --orchestrator)")
	}

	token := os.Getenv("AMUX_PROVIDER_TOKEN")
	if f.tokenFile != "" {
		b, err := os.ReadFile(f.tokenFile)
		if err != nil {
			return fmt.Errorf("provide: read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}

	displayName := f.name
	if displayName == "" {
		displayName = os.Getenv("AMUX_PROVIDER_NAME")
	}
	if displayName == "" {
		displayName, _ = os.Hostname()
	}

	ca := f.caFile
	if ca == "" {
		ca = os.Getenv(wiretls.EnvCA)
	}
	sni := f.serverName
	if sni == "" {
		sni = os.Getenv(wiretls.EnvServer)
	}

	mp := f.maxPanes
	if mp == 0 {
		if s := os.Getenv("AMUX_PROVIDER_MAX_PANES"); s != "" {
			mp, _ = strconv.Atoi(s)
		}
	}

	publish := f.publishSes || envBool("AMUX_PROVIDER_PUBLISH_SESSIONS")
	readonly := f.readOnly || envBool("AMUX_PROVIDER_SESSIONS_READONLY")
	runtimeEvents := f.rtEvents || envBool("AMUX_PROVIDER_RUNTIME_EVENTS")

	cfg := provider.Config{
		Orchestrator: addr,
		Token:        token,
		Name:         displayName,
		Labels:       parseLabels(os.Getenv("AMUX_PROVIDER_LABELS"), f.labels),
		CAFile:       ca,
		ServerName:   sni,
		MaxPanes:     mp,
		Features:     mergeFeatures(os.Getenv("AMUX_PROVIDER_FEATURES"), f.features),
		Logf:         func(format string, a ...any) { fmt.Fprintf(os.Stderr, "amux provide: "+format+"\n", a...) },
	}
	if publish {
		// The published inventory and transcript paths come from the daemon — the
		// single store owner — over its socket, the same authority lifecycle verbs
		// already use (applyViaDaemon). The provider process never opens the store
		// itself; with no daemon reachable, publishing degrades cleanly (a poll error
		// publishes nothing, a resolve miss emits no events).
		cfg.PublishSessions = true
		cfg.ReadOnlySessions = readonly
		cfg.Sessions = sessionsViaDaemon
		if !readonly {
			cfg.ApplyAction = applyViaDaemon
		}
		if runtimeEvents {
			// Structured transcripts: tail each published session's on-disk runtime
			// record (Claude Code JSONL, located via the conversation id amux pins)
			// and stream contract events. Read-only; a session with no record on disk
			// simply emits nothing (honest degradation).
			cfg.RuntimeEvents = true
			cfg.RuntimeEventStream = runtimeevents.ClaudeStream(runtimePathViaDaemon(), 0)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.New(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// provideFlags is provider mode's settings surface. It is a struct registered on
// a FlagSet rather than a pile of pointers in cmdProvide so the parse can be
// exercised on its own — which is what tells the flags-in-any-order fix from a
// regression back to silently dropping them.
type provideFlags struct {
	orch       string
	tokenFile  string
	name       string
	caFile     string
	serverName string
	maxPanes   int
	publishSes bool
	readOnly   bool
	rtEvents   bool
	labels     multiFlag
	features   multiFlag
}

func (f *provideFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.orch, "orchestrator", "", "orchestrator address host:port (or as the positional arg)")
	fs.StringVar(&f.tokenFile, "token-file", "", "file holding the bearer token (mode 0600); preferred over $AMUX_PROVIDER_TOKEN")
	fs.StringVar(&f.name, "name", "", "provider display name (default $AMUX_PROVIDER_NAME or hostname)")
	fs.StringVar(&f.caFile, "ca", "", "PEM CA file to trust in addition to the system roots (default $AMUX_TLS_CA)")
	fs.StringVar(&f.serverName, "server-name", "", "TLS server name for SNI/verification (default $AMUX_TLS_SERVERNAME)")
	fs.IntVar(&f.maxPanes, "max-panes", 0, "capability: max concurrent panes (default $AMUX_PROVIDER_MAX_PANES)")
	fs.BoolVar(&f.publishSes, "publish-sessions", false, "advertise the sessions feature: publish this daemon's session inventory and accept lifecycle verbs (default $AMUX_PROVIDER_PUBLISH_SESSIONS)")
	fs.BoolVar(&f.readOnly, "read-only-sessions", false, "publish inventory but reject every lifecycle verb (default $AMUX_PROVIDER_SESSIONS_READONLY)")
	fs.BoolVar(&f.rtEvents, "runtime-events", false, "additionally stream read-only structured transcript events for published sessions from the local runtime's session record (default $AMUX_PROVIDER_RUNTIME_EVENTS); requires --publish-sessions")
	fs.Var(&f.labels, "label", "scheduling label key=value (repeatable); merged over $AMUX_PROVIDER_LABELS")
	fs.Var(&f.features, "feature", "capability feature string (repeatable); merged with $AMUX_PROVIDER_FEATURES")
}

// provideAddr picks the orchestrator address out of --orchestrator and whatever
// operands were left over. One address is expected, from either spelling; the two
// ways of getting that wrong both used to pass unnoticed, so both now say so:
// a second, different address is ambiguous, and extra operands are the shape of a
// typo (a missing `--`, a flag misspelled into a word). Returns "" when no address
// was given at all — the caller decides what else may supply one.
func provideAddr(orch string, operands []string) (string, error) {
	if len(operands) > 1 {
		return "", fmt.Errorf("provide: unexpected argument(s) after the orchestrator address %q: %s", operands[0], strings.Join(operands[1:], " "))
	}
	if len(operands) == 0 {
		return orch, nil
	}
	if orch != "" && orch != operands[0] {
		return "", fmt.Errorf("provide: two orchestrator addresses: --orchestrator %s and %s — give it once", orch, operands[0])
	}
	return operands[0], nil
}

// envBool reports whether an env var is set to a truthy value (1/true/yes/on).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sessionsViaDaemon fetches the published session rail from the local daemon —
// the store owner — instead of opening the store in this process. The daemon
// serves its already-computed snapshot (engine liveness baked in), so the
// inventory the provider publishes matches exactly what the daemon shows. It dials
// fresh per poll; an unreachable daemon returns an error the provider handles by
// publishing nothing that cycle.
func sessionsViaDaemon(ctx context.Context) ([]core.Session, error) {
	c, err := daemon.Dial()
	if err != nil {
		return nil, fmt.Errorf("daemon unreachable: %w", err)
	}
	defer c.Close()
	return c.Snapshot()
}

// runtimePathViaDaemon resolves a published session id to its on-disk transcript
// path through the daemon (which resolves it via the session's harness), so the
// provider tails transcripts without opening the store. ok=false — a missing
// daemon, an error, or an empty path (no supported record) — means the provider
// advertises runtime-events but honestly emits nothing for that session.
func runtimePathViaDaemon() runtimeevents.PathResolver {
	return func(sessionID string) (string, bool) {
		if sessionID == "" {
			return "", false
		}
		c, err := daemon.Dial()
		if err != nil {
			return "", false
		}
		defer c.Close()
		path, err := c.RuntimePath(sessionID)
		if err != nil || path == "" {
			return "", false
		}
		return path, true
	}
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
