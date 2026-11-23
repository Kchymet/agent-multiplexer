package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"amux/internal/direct"
)

// directEnrollExample is a complete, working enroll command line, quoted in
// errors so a refused invocation always shows one that would work.
const directEnrollExample = "amux direct enroll --label phone --permissions read"

// enrollEnv is everything enroll touches outside its arguments, so tests can
// drive it without a terminal, a daemon or a direct server.
type enrollEnv struct {
	stdout    io.Writer
	stderr    io.Writer
	stderrTTY bool
	// openTTY opens the controlling terminal for the QR when stderr is not one.
	openTTY  func() (io.WriteCloser, error)
	getenv   func(string) string
	hostname func() (string, error)
	now      func() time.Time
	admin    func(direct.AdminRequest) (direct.AdminResponse, error)
}

func osEnrollEnv() enrollEnv {
	return enrollEnv{
		stdout: os.Stdout, stderr: os.Stderr, stderrTTY: isTerminal(os.Stderr),
		openTTY:  func() (io.WriteCloser, error) { return os.OpenFile("/dev/tty", os.O_WRONLY, 0) },
		getenv:   os.Getenv,
		hostname: os.Hostname,
		now:      time.Now,
		admin:    adminRun,
	}
}

func cmdDirectEnroll(args []string) error { return runDirectEnroll(args, osEnrollEnv()) }

type enrollFlags struct {
	label, perms string
	all          bool
	groups       multiFlag
	ttl          time.Duration
	asJSON       bool
	qr, invert   bool
	qrPNG        string
}

func parseEnrollFlags(args []string) (enrollFlags, error) {
	var f enrollFlags
	fs := newCmdFlagSet("direct enroll")
	fs.StringVar(&f.label, "label", "", "")
	fs.StringVar(&f.perms, "permissions", "", "")
	fs.BoolVar(&f.all, "all-sessions", false, "")
	fs.DurationVar(&f.ttl, "ttl", 0, "")
	fs.BoolVar(&f.asJSON, "json", false, "")
	fs.BoolVar(&f.qr, "qr", false, "")
	fs.BoolVar(&f.invert, "invert", false, "")
	fs.StringVar(&f.qrPNG, "qr-png", "", "")
	fs.Var(&f.groups, "workgroup", "")
	if err := parseCmdFlags(fs, "direct enroll", args); err != nil {
		return f, err
	}
	if fs.NArg() != 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if f.invert && !f.qr {
		return f, errors.New("--invert applies to --qr")
	}
	pngSet := false
	fs.Visit(func(fl *flag.Flag) { pngSet = pngSet || fl.Name == "qr-png" })
	if pngSet && f.qrPNG == "" {
		return f, errors.New("--qr-png needs a file path")
	}
	return f, nil
}

// enrollGrant is what one enrollment will grant, with which parts defaulted.
type enrollGrant struct {
	label           string
	labelDefault    bool
	perms           []string
	permsDefault    bool
	resource        direct.ResourceScope
	resourceDefault bool
}

func (g enrollGrant) describe() string {
	note := func(d bool) string {
		if d {
			return " (default)"
		}
		return ""
	}
	scope := "all sessions on this host"
	if g.resource.Kind == direct.ResourceWorkgroups {
		scope = "workgroup " + strings.Join(g.resource.Workgroups, ", ")
	}
	return fmt.Sprintf("Granting: label %q%s · permissions %s%s · scope %s%s",
		g.label, note(g.labelDefault), strings.Join(g.perms, ","), note(g.permsDefault), scope, note(g.resourceDefault))
}

// defaultEnrollLabel names a client after this host and the current minute.
func defaultEnrollLabel(hostname func() (string, error), now time.Time) string {
	host := "client"
	if h, err := hostname(); err == nil {
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
		h = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, h)
		if h = strings.TrimSpace(h); h != "" {
			if len(h) > 40 {
				h = h[:40]
			}
			host = h
		}
	}
	return host + "-" + now.Format("20060102-1504")
}

// resolveEnrollGrant applies the defaults: label from the host and time,
// permissions read, and scope every ordinary session (never the console,
// which no scope can name). --workgroup narrows the scope.
func resolveEnrollGrant(f enrollFlags, env enrollEnv) (enrollGrant, error) {
	g := enrollGrant{label: f.label, perms: nil}
	if g.label == "" {
		g.label, g.labelDefault = defaultEnrollLabel(env.hostname, env.now()), true
	}
	if f.perms == "" {
		g.perms, g.permsDefault = []string{direct.PermRead}, true
	} else {
		for _, p := range strings.Split(f.perms, ",") {
			g.perms = append(g.perms, strings.TrimSpace(p))
		}
	}
	res, err := directResourceFlags(f.all, f.groups)
	if err != nil {
		return g, err
	}
	g.resource, g.resourceDefault = res, !f.all && len(f.groups) == 0
	return g, nil
}

func runDirectEnroll(args []string, env enrollEnv) error {
	for _, a := range args {
		if a == "-h" || a == "-help" || a == "--help" {
			printHelp(env.stdout, mustHelpNode("direct enroll"))
			return nil
		}
	}
	f, err := parseEnrollFlags(args)
	if err != nil {
		return err
	}
	g, err := resolveEnrollGrant(f, env)
	if err != nil {
		return err
	}
	ticket := f.qr || f.qrPNG != ""

	// Everything that can fail locally fails before a credential exists.
	var qrOut io.Writer
	if f.qr {
		if env.stderrTTY {
			qrOut = env.stderr
		} else {
			tty, err := env.openTTY()
			if err != nil {
				return errors.New("--qr needs a terminal (stderr or /dev/tty) to draw on; use --qr-png <path> instead")
			}
			defer tty.Close()
			qrOut = tty
		}
	}
	var png *os.File
	if f.qrPNG != "" {
		if png, err = createQRPNG(f.qrPNG); err != nil {
			return err
		}
	}
	discardPNG := func() {
		if png != nil {
			png.Close()
			os.Remove(f.qrPNG)
		}
	}

	fmt.Fprintln(env.stderr, g.describe())
	op := direct.OpEnroll
	if ticket {
		op = direct.OpEnrollTicket
	}
	resp, err := env.admin(direct.AdminRequest{
		Op: op, Label: g.label, Permissions: g.perms, Resource: g.resource,
		TTLSeconds: int64(f.ttl / time.Second),
	})
	if err != nil {
		discardPNG()
		if ticket && strings.Contains(err.Error(), "unknown admin operation") {
			return errors.New("the running direct server predates QR enrollment; restart it (amux direct disable && amux direct enable) and retry")
		}
		return err
	}
	if ticket && !strings.HasPrefix(resp.Bundle, direct.BundlePrefixTicket) {
		discardPNG()
		return errors.New("the direct server did not return a device ticket; nothing was drawn")
	}
	ttl := time.Unix(resp.ExpiresAt, 0).Sub(env.now()).Round(time.Second)

	if png != nil {
		if err := writeQRPNG(png, resp.Bundle); err != nil {
			os.Remove(f.qrPNG)
			return err
		}
		fmt.Fprintf(env.stderr, "Wrote the QR code to %s (mode 0600). It is a live credential until used or expired: delete it after enrolling (rm %s).\n", f.qrPNG, f.qrPNG)
	}
	if qrOut != nil {
		fmt.Fprintf(qrOut, "\nScan with the Harness Android app (Backends > Connect a machine from a QR code) within %s.\nSingle use; server_id %s. The same ticket is on stdout.\n\n", ttl, resp.ServerID)
		if err := renderQRHalfBlocks(qrOut, resp.Bundle, f.invert, terminalColors(env.getenv)); err != nil {
			return err
		}
		fmt.Fprintln(qrOut)
	}
	if f.asJSON {
		return json.NewEncoder(env.stdout).Encode(resp)
	}
	if qrOut == nil {
		if ticket {
			fmt.Fprintf(env.stderr, "Device ticket for the Harness Android app, valid %s (single use; server_id %s):\n\n", ttl, resp.ServerID)
		} else {
			fmt.Fprintf(env.stderr, "Paste this into the client within %s (single use; server_id %s):\n\n", ttl, resp.ServerID)
		}
	}
	_, err = fmt.Fprintln(env.stdout, resp.Bundle)
	return err
}
