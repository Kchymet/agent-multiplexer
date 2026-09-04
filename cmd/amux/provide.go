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
	"amux/internal/providercfg"
	"amux/internal/runtimeevents"
	"amux/internal/wiretls"
)

// cmdProvide is the provider-mode namespace: run the provider in the
// foreground, or install/uninstall it as a user service.
//
//	amux provide [<orchestrator-addr>] [flags]   run (reads the config file when bare)
//	amux provide install [flags]                 write the config + install the service
//	amux provide uninstall                       remove the service
//
// Flags work on either side of the address (parseFlagsAnyOrder), for running and
// for installing alike — the address-first phrasing is the one people reach for,
// and it used to drop every flag after the address without a word.
func cmdProvide(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if isHelpFlag(sub) {
		provideUsage()
		return nil
	}
	if run, ok := provideSubcommands[sub]; ok {
		return run(args[1:])
	}
	return provideRun(args)
}

// provideSubcommands is the provider-mode verb dispatch. It is a table, like
// daemonSubcommands, so the CLI contract test can check the advertised verbs by
// name without running them — installing a system service is not something a
// test should do for real.
var provideSubcommands = map[string]func([]string) error{
	"install":   cmdProvideInstall,
	"uninstall": cmdProvideUninstall,
}

func provideUsage() {
	fmt.Fprint(os.Stderr, `amux provide — run this machine as a remote compute provider

Provider mode dials out to a remote orchestrator over TLS, registers this
machine as a compute node, and serves agent panes over that one connection.
See docs/remote-provider.md.

usage: amux provide [<orchestrator-addr>] [flags]
       amux provide <command>

  (bare)             run in the foreground, reading `+"`"+`amux provide install`+"`"+`'s config file
  install            write the config file and install the user service
  uninstall          stop and remove the user service (the config file is kept)

Install writes `+"`"+`~/.config/amux/provider.toml`+"`"+` and a user service (a systemd user
unit on Linux/WSL2, a launchd agent on macOS), so the provider survives reboots
and no terminal has to stay open. The bearer token is never written to the
config or passed in argv — it stays in the 0600 file named by --token-file, so
rotating it is one write and no reinstall.

  --orchestrator <host:port>  orchestrator to dial (or give it as the positional arg)
  --token-file <path>  file holding the bearer token (mode 0600)
  --name <text>      provider display name (default: hostname)
  --ca <pem>         private CA to trust on top of the system roots
  --server-name <n>  TLS server name for SNI/verification
  --max-panes <n>    capability: max concurrent panes
  --label k=v        scheduling label (repeatable)
  --feature <s>      opaque capability feature string (repeatable)
  --publish-sessions  publish this daemon's session inventory and accept lifecycle verbs
  --read-only-sessions  publish inventory but reject every lifecycle verb
  --runtime-events   stream read-only transcript events (needs --publish-sessions)

Install-only flags:

  --dry-run          print the config file and service unit, write nothing
  --exec <path>      amux binary the service runs (default: the installed binary)

Flags may come before or after the address, for install and for running alike.

Running bare, settings resolve flags first, then the AMUX_PROVIDER_* / AMUX_TLS_*
env vars, then the config file. `+"`"+`amux doctor`+"`"+` reports the config, the service, and
the provider's last registration and heartbeat.

  amux provide install --orchestrator orch.example.com:7443 \
                       --token-file ~/.config/amux/provider.token
  amux doctor
`)
}

// provideRun runs provider mode in the foreground: dial out to a remote
// orchestrator over TLS, register this machine as a compute node, and serve
// harnessproto v2 (spawn/input/resize/kill ⇄ output/exit) over the connection.
// Panes survive reconnects within the orchestrator's grace window. See
// docs/remote-provider.md.
//
// Settings resolve from flags, then the AMUX_PROVIDER_* / AMUX_TLS_* env vars,
// then the config file `amux provide install` wrote — so a bare `amux provide`
// (what the user service runs) is fully configured by that file, while an ad-hoc
// invocation can still override any single setting. The token is never taken
// from argv (it is a bearer credential): use --token-file or AMUX_PROVIDER_TOKEN.
func provideRun(args []string) error {
	fs := flag.NewFlagSet("provide", flag.ContinueOnError)
	var f provideFlags
	f.register(fs)
	operands, err := parseFlagsAnyOrder(fs, args)
	if err != nil {
		return err
	}

	// A missing config file is normal (provider mode is opt-in and works fully
	// from flags); a malformed one is not, and must not be silently ignored.
	file, ferr := providercfg.Load()
	if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
		return fmt.Errorf("provide: %w", ferr)
	}

	addr, err := provideAddr(f.orch, operands)
	if err != nil {
		return err
	}
	if addr == "" {
		addr = file.Orchestrator // the config file is what the user service runs on
	}
	if addr == "" {
		return fmt.Errorf("provide: need an orchestrator address (positional, --orchestrator, or `amux provide install`)")
	}

	tokenFile := firstNonEmpty(f.tokenFile, file.TokenFile)
	token := os.Getenv("AMUX_PROVIDER_TOKEN")
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return fmt.Errorf("provide: read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}

	displayName := firstNonEmpty(f.name, os.Getenv("AMUX_PROVIDER_NAME"), file.Name)
	if displayName == "" {
		displayName, _ = os.Hostname()
	}

	ca := firstNonEmpty(f.caFile, os.Getenv(wiretls.EnvCA), file.CAFile)
	sni := firstNonEmpty(f.serverName, os.Getenv(wiretls.EnvServer), file.ServerName)

	mp := f.maxPanes
	if mp == 0 {
		if s := os.Getenv("AMUX_PROVIDER_MAX_PANES"); s != "" {
			mp, _ = strconv.Atoi(s)
		}
	}
	if mp == 0 {
		mp = file.MaxPanes
	}

	publish := f.publishSes || envBool("AMUX_PROVIDER_PUBLISH_SESSIONS") || file.PublishSessions
	readonly := f.readOnly || envBool("AMUX_PROVIDER_SESSIONS_READONLY") || file.ReadOnlySessions
	runtimeEvents := f.rtEvents || envBool("AMUX_PROVIDER_RUNTIME_EVENTS") || file.RuntimeEvents

	cfg := provider.Config{
		Orchestrator: addr,
		Token:        token,
		Name:         displayName,
		Labels:       mergeLabels(file.Labels, parseLabels(os.Getenv("AMUX_PROVIDER_LABELS"), f.labels)),
		CAFile:       ca,
		ServerName:   sni,
		MaxPanes:     mp,
		Features:     mergeFeatures(os.Getenv("AMUX_PROVIDER_FEATURES"), append(multiFlag(file.Features), f.features...)),
		Logf:         func(format string, a ...any) { fmt.Fprintf(os.Stderr, "amux provide: "+format+"\n", a...) },
		// The status file is how `amux doctor` — and anyone looking at a headless
		// provider box — sees whether this thing is actually connected, instead of
		// inferring it from a log tail.
		OnStatus: func(st provider.Status) { _ = provider.WriteStatus(provider.StatusPath(), st) },
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
			// record — Claude Code's session JSONL or Codex's rollout, whichever the
			// session's harness wrote — and stream contract events stamped with that
			// runtime. Read-only; a session with no record on disk simply emits
			// nothing (honest degradation).
			cfg.RuntimeEvents = true
			cfg.RuntimeEventStream = runtimeevents.Stream(runtimeRecordViaDaemon(), 0)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.New(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// provideFlags is provider mode's settings surface, shared by running the
// provider and installing it — so the flags mean exactly the same thing in both,
// and `amux provide install` is the run command with its arguments written down.
// It is a struct registered on a FlagSet rather than a pile of pointers in
// cmdProvide so the parse can be exercised on its own — which is what tells the
// flags-in-any-order fix from a regression back to silently dropping them.
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

// runtimeRecordViaDaemon resolves a published session id to its on-disk
// transcript and the runtime that wrote it, through the daemon (which resolves it
// via the session's harness), so the provider tails transcripts without opening
// the store. ok=false — a missing daemon, an error, or an empty path (no
// supported record) — means the provider advertises runtime-events but honestly
// emits nothing for that session.
func runtimeRecordViaDaemon() runtimeevents.Resolver {
	return func(sessionID string) (runtimeevents.Record, bool) {
		if sessionID == "" {
			return runtimeevents.Record{}, false
		}
		c, err := daemon.Dial()
		if err != nil {
			return runtimeevents.Record{}, false
		}
		defer c.Close()
		rec, err := c.RuntimeRecord(sessionID)
		if err != nil || rec.Path == "" {
			return runtimeevents.Record{}, false
		}
		return runtimeevents.Record{Runtime: rec.Runtime, Path: rec.Path}, true
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
