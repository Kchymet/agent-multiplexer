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

	"amux/internal/direct"
	"amux/internal/hostbackend"
	"amux/internal/provider"
	"amux/internal/providercfg"
	"amux/internal/runtimeevents"
	"amux/internal/wiretls"
)

// cmdProvide is the provider-mode namespace: run the provider in the
// foreground, or install/uninstall it as a user service.
//
//	amux provide [<orchestrator-addr>] [flags]   run (reads the config file when bare)
//	amux provide [flags] [<orchestrator-addr>]   the same run, flags on the other side
//	amux provide install [flags]                 write the config + install the service
//	amux provide uninstall                       remove the service
//
// Flags work on either side of the address (parseFlagsAnyOrder) — the first
// phrasing above is the one people reach for, and it used to drop every flag
// after the address without a word.
//
// Config resolves from flags, then the AMUX_PROVIDER_* / AMUX_TLS_* env vars,
// then the config file `amux provide install` wrote. The token is never taken
// from argv (a bearer credential): use --token-file or AMUX_PROVIDER_TOKEN.
func cmdProvide(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	if isHelpFlag(sub) {
		return showHelp("provide")
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
	"revoke":    cmdProvideRevoke,
}

// provideFlags is the settings surface shared by running the provider and
// installing it, so the flags mean exactly the same thing in both — the install
// command is the run command with its arguments written down. It is a struct
// registered on a FlagSet rather than a pile of pointers in cmdProvide so the
// parse can be exercised on its own — which is what tells the flags-in-any-order
// fix from a regression back to silently dropping them.
type provideFlags struct {
	accountStatus, accountManage, accountUse bool
	orch                                     string
	tokenFile                                string
	name                                     string
	caFile                                   string
	serverName                               string
	maxPanes                                 int
	allowCompute                             bool
	publishSes                               bool
	readOnly                                 bool
	rtEvents                                 bool
	directEnroll                             bool
	directWake                               bool
	labels                                   multiFlag
	features                                 multiFlag
	harnesses                                multiFlag
	identityMode                             string
}

func (f *provideFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.accountStatus, "account-status", false, "")
	fs.BoolVar(&f.accountManage, "account-manage", false, "")
	fs.BoolVar(&f.accountUse, "account-use", false, "")
	fs.StringVar(&f.orch, "orchestrator", "", "")
	fs.StringVar(&f.tokenFile, "token-file", "", "")
	fs.StringVar(&f.name, "name", "", "")
	fs.StringVar(&f.caFile, "ca", "", "")
	fs.StringVar(&f.serverName, "server-name", "", "")
	fs.IntVar(&f.maxPanes, "max-panes", 0, "")
	fs.BoolVar(&f.allowCompute, "allow-compute", false, "")
	fs.BoolVar(&f.publishSes, "publish-sessions", false, "")
	fs.BoolVar(&f.readOnly, "read-only-sessions", false, "")
	fs.BoolVar(&f.rtEvents, "runtime-events", false, "")
	fs.BoolVar(&f.directWake, "direct-wake", false, "")
	fs.BoolVar(&f.directEnroll, "direct-enroll", false, "")
	fs.Var(&f.labels, "label", "")
	fs.Var(&f.harnesses, "harness", "")
	fs.StringVar(&f.identityMode, "identity-mode", "", "")
	fs.Var(&f.features, "feature", "")
}

// provideRun runs provider mode in the foreground: dial out to a remote
// orchestrator over TLS, register this machine (with Harness, an owned
// registration is the user's personal compute pool of kind machine), and serve
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
	fs := newCmdFlagSet("provide")
	var f provideFlags
	f.register(fs)
	operands, err := parseCmdFlagsAnyOrder(fs, "provide", args)
	if err != nil {
		return err
	}

	if err := rejectRemoteAccountManagement(f.accountManage); err != nil {
		return err
	}

	// A missing config file is normal (provider mode is opt-in and works fully
	// from flags); a malformed one is not, and must not be silently ignored.
	file, err := providercfg.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("provide: %w", err)
	}

	// The address may come from either side of the flags, from --orchestrator, or
	// — for the bare `amux provide` the user service runs — from the config file.
	addr, err := provideAddr(f.orch, operands)
	if err != nil {
		return err
	}
	addr = firstNonEmpty(addr, file.Orchestrator)
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
	allowCompute := resolvedBool(fs, "allow-compute", f.allowCompute, "AMUX_PROVIDER_ALLOW_COMPUTE", file.AllowCompute)

	execution, err := executionConfig(f, file, os.Getenv)
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "amux provide: "+format+"\n", a...) }
	cfg := provider.Config{
		AccountStatus:     resolvedBool(fs, "account-status", f.accountStatus, "AMUX_PROVIDER_ACCOUNT_STATUS", file.AccountStatus),
		AccountManage:     false, // legacy saved/env grants cannot enable remote configuration
		AccountUse:        resolvedBool(fs, "account-use", f.accountUse, "AMUX_PROVIDER_ACCOUNT_USE", providerAccountUseDefault(file, publish, readonly)),
		DiscoverExecution: executionDiscovery(execution, logf),
		Orchestrator:      addr,
		Token:             token,
		Name:              displayName,
		Labels:            mergeLabels(file.Labels, parseLabels(os.Getenv("AMUX_PROVIDER_LABELS"), f.labels)),
		CAFile:            ca,
		ServerName:        sni,
		MaxPanes:          mp,
		AllowCompute:      allowCompute,
		Features:          mergeFeatures(os.Getenv("AMUX_PROVIDER_FEATURES"), append(multiFlag(file.Features), f.features...)),
		Logf:              logf,
		// The status file is how `amux doctor` — and anyone looking at a headless
		// provider box — sees whether this thing is actually connected, instead of
		// inferring it from a log tail.
		OnStatus: func(st provider.Status) { _ = provider.WriteStatus(provider.StatusPath(), st) },
	}
	host := hostbackend.New()
	wireHostBackend(&cfg, host, publish, readonly, runtimeEvents)
	wireSessionIncarnation(&cfg, host)
	if resolvedBool(fs, "direct-wake", f.directWake, "AMUX_PROVIDER_DIRECT_WAKE", file.DirectWake) {
		wireDirectWake(&cfg)
	}
	if resolvedBool(fs, "direct-enroll", f.directEnroll, "AMUX_PROVIDER_DIRECT_ENROLL", file.DirectEnroll) {
		wireDirectEnroll(&cfg)
	}

	// One provider per host owns the status file; a second would overwrite the
	// registration doctor reports and direct cloud enable defaults its pins to.
	releaseStatus, err := provider.LockStatus(provider.StatusPath())
	if err != nil {
		return err
	}
	defer releaseStatus()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := provider.New(cfg).Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
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

// resolvedBool applies the documented flag > environment > file precedence for
// a security-sensitive boolean. Presence matters: an explicit false flag or
// environment value must be able to revoke a true installed setting.
func resolvedBool(fs *flag.FlagSet, flagName string, flagValue bool, envName string, fileValue bool) bool {
	flagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == flagName {
			flagSet = true
		}
	})
	if flagSet {
		return flagValue
	}
	if _, ok := os.LookupEnv(envName); ok {
		return envBool(envName)
	}
	return fileValue
}

// wireHostBackend routes every session-facing provider hook to the shared local
// host backend — the daemon, the single store and engine owner — over its
// authenticated socket. The provider process never opens the store itself; with
// no daemon reachable, publishing degrades cleanly (a poll error publishes
// nothing, a resolve miss emits no events). Account access always goes through
// it; inventory, lifecycle verbs and transcripts only when published.
func wireHostBackend(cfg *provider.Config, h hostbackend.HostDaemon, publish, readOnly, runtimeEvents bool) {
	cfg.OpenAccounts = func(ctx context.Context) (provider.AccountChannel, error) {
		a, err := h.OpenAccounts(ctx)
		if err != nil {
			return nil, err
		}
		return a, nil
	}
	if !publish {
		return
	}
	cfg.PublishSessions = true
	cfg.ReadOnlySessions = readOnly
	cfg.Sessions = h.Snapshot
	cfg.EnrichmentKinds = h.EnrichmentKinds
	if !readOnly {
		cfg.ApplyAction = h.Apply
	}
	if runtimeEvents {
		// Structured transcripts: tail each published session's on-disk runtime
		// record — Claude Code's session JSONL or Codex's rollout, whichever the
		// session's harness wrote — and stream contract events stamped with that
		// runtime. Read-only; a session with no record on disk simply emits
		// nothing (honest degradation).
		cfg.RuntimeEvents = true
		cfg.RuntimeEventStream = runtimeevents.StreamContext(runtimeRecordResolver(h), 0)
		// Newest-first pages of the same records, so an orchestrator can show
		// the latest page at once and fetch older history only on demand.
		cfg.RuntimeEventHistory = runtimeevents.HistoryContext(runtimeRecordResolver(h))
	}
}

// runtimeRecordResolver resolves a published session id to its on-disk
// transcript and the runtime that wrote it, through the host backend, so the
// provider tails transcripts without opening the store. ok=false — a missing
// daemon, an error, or a record naming no file at all — means the provider
// advertises runtime-events but honestly emits nothing for that session.
//
// A record with no transcript but an amux journal is still worth tailing: that is
// a session which has not run yet, and an accepted `prompt` cold-starting it
// reports its progress into that journal before any transcript exists.
func runtimeRecordResolver(h hostbackend.HostDaemon) runtimeevents.ContextResolver {
	return func(ctx context.Context, sessionID string) (runtimeevents.Record, bool) {
		if sessionID == "" || ctx.Err() != nil {
			return runtimeevents.Record{}, false
		}
		rec, err := h.RuntimeRecord(ctx, sessionID)
		if err != nil || (rec.Path == "" && rec.Journal == "") {
			return runtimeevents.Record{}, false
		}
		return runtimeevents.Record{
			Runtime: rec.Runtime, Path: rec.Path,
			Permissions: rec.Permissions, Journal: rec.Journal,
			Structured: rec.Structured, PermissionBindings: rec.PermissionBindings,
		}, true
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

// wireDirectEnroll offers direct-enroll-v1. The provider already acts with the
// host credential for the local daemon; this bridge narrows what the relay can
// reach on the direct server's host-only admin channel to exactly two typed
// operations (status for advertisement, cloud-code issuance), and the direct
// server enforces the operator's policy, pins, ceiling, PKCE and caps.
func wireDirectEnroll(cfg *provider.Config) {
	cfg.DirectEnroll = direct.NewRelayIssuer(func(ctx context.Context, req direct.AdminRequest) (direct.AdminResponse, error) {
		cred, err := hostCredential()
		if err != nil {
			return direct.AdminResponse{}, err
		}
		root, err := directRoot()
		if err != nil {
			return direct.AdminResponse{}, err
		}
		return direct.AdminCall(ctx, root, cred, req)
	})
}
