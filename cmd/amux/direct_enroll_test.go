package main

import (
	"bytes"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/makiuchi-d/gozxing"
	gozxingqr "github.com/makiuchi-d/gozxing/qrcode"

	"amux/internal/direct"
)

// Every bundle here is synthetic: fixed filler values that no server ever
// issued, so no test can leak or depend on a real credential.
func fakeBundle(t *testing.T, v int, endpoints ...string) string {
	t.Helper()
	if len(endpoints) == 0 {
		endpoints = []string{"https://macbook-pro.tail1a2b3c.ts.net:8443"}
	}
	b := direct.Bundle{V: v, ServerID: "TESTserverIDtestSERVERidTESTserverIDtest-_0", Endpoints: endpoints,
		Code: "TESTcodeNOTaREALcredentialTESTcodeNOTreal_0", ExpiresAt: 1_900_000_000}
	if v == 2 {
		b.Claim = direct.BundleClaimPKCE
	}
	s, err := direct.EncodeBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type enrollHarness struct {
	env      enrollEnv
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	tty      *bytes.Buffer
	requests []direct.AdminRequest
	resp     direct.AdminResponse
	err      error
}

func newEnrollHarness(t *testing.T) *enrollHarness {
	h := &enrollHarness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, tty: &bytes.Buffer{}}
	now := time.Date(2026, 11, 22, 15, 30, 0, 0, time.UTC)
	h.env = enrollEnv{
		stdout: h.stdout, stderr: h.stderr,
		openTTY:  func() (io.WriteCloser, error) { return nopWriteCloser{h.tty}, nil },
		getenv:   func(string) string { return "" },
		hostname: func() (string, error) { return "studio.local", nil },
		now:      func() time.Time { return now },
		admin: func(req direct.AdminRequest) (direct.AdminResponse, error) {
			h.requests = append(h.requests, req)
			if h.err != nil {
				return direct.AdminResponse{}, h.err
			}
			r := h.resp
			if r.Bundle == "" {
				v := 1
				if req.Op == direct.OpEnrollTicket {
					v = 2
				}
				r = direct.AdminResponse{OK: true, Bundle: fakeBundle(t, v), ServerID: "srv", ExpiresAt: now.Add(10 * time.Minute).Unix()}
			}
			return r, nil
		},
	}
	return h
}

func TestEnrollFlagParsing(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"--all-sessions"}, true},
		{[]string{"--qr", "--invert", "--all-sessions"}, true},
		{[]string{"--qr-png", "/x/y.png", "--all-sessions"}, true},
		{[]string{"--invert", "--all-sessions"}, false}, // --invert needs --qr
		{[]string{"--qr-png=", "--all-sessions"}, false},
		{[]string{"--all-sessions", "extra"}, false},
		{[]string{"--bogus"}, false},
	} {
		_, err := parseEnrollFlags(tc.args)
		if (err == nil) != tc.ok {
			t.Errorf("%v: err = %v, want ok=%v", tc.args, err, tc.ok)
		}
	}
	f, err := parseEnrollFlags([]string{"--qr", "--invert", "--qr-png", "p.png", "--label", "L", "--permissions", "read,steer", "--workgroup", "a", "--workgroup", "b", "--ttl", "5m", "--json"})
	if err != nil || !f.qr || !f.invert || f.qrPNG != "p.png" || f.label != "L" || f.perms != "read,steer" ||
		len(f.groups) != 2 || f.ttl != 5*time.Minute || !f.asJSON {
		t.Fatalf("parsed %+v %v", f, err)
	}
}

func TestEnrollHelpDocumentsEveryFlag(t *testing.T) {
	h := newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--help"}, h.env); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--label", "--permissions", "--all-sessions", "--workgroup", "--ttl", "--json", "--qr", "--invert", "--qr-png", "/dev/tty", directEnrollExample[:18]} {
		if !strings.Contains(h.stdout.String(), flag) {
			t.Errorf("enroll --help does not mention %s", flag)
		}
	}
	if len(h.requests) != 0 {
		t.Fatal("help minted a code")
	}
	if out := h.stdout.String(); !strings.Contains(out, "(default: read)") || !strings.Contains(out, "every ordinary session") ||
		!strings.Contains(out, "console is never in any scope") {
		t.Fatal("help does not state the defaults")
	}
}

func TestEnrollExampleParses(t *testing.T) {
	f, err := parseEnrollFlags(strings.Fields(directEnrollExample)[3:])
	if err != nil || f.all || len(f.groups) != 0 || f.label != "phone" || f.perms != "read" {
		t.Fatalf("example does not parse: %+v %v", f, err)
	}
}

// With no scope flag, enroll grants every ordinary session, on or off a
// terminal and with or without --qr, and says the scope was defaulted. The
// console is never named: all_sessions excludes it.
func TestEnrollScopeDefaultsToAllSessions(t *testing.T) {
	for _, args := range [][]string{nil, {"--permissions", "read,steer"}, {"--qr-png", filepath.Join(t.TempDir(), "code.png")}} {
		h := newEnrollHarness(t)
		if err := runDirectEnroll(args, h.env); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if len(h.requests) != 1 {
			t.Fatalf("%v: %d requests", args, len(h.requests))
		}
		got := h.requests[0].Resource
		if got.Kind != direct.ResourceAllSessions || len(got.Workgroups) != 0 {
			t.Errorf("%v: resource %+v", args, got)
		}
		if !strings.Contains(h.stderr.String(), "scope all sessions on this host (default)") {
			t.Errorf("%v: defaulted scope not shown: %q", args, h.stderr.String())
		}
	}
	// An explicit --all-sessions is not marked as a default.
	h := newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--all-sessions"}, h.env); err != nil {
		t.Fatal(err)
	}
	if out := h.stderr.String(); strings.Contains(out, "scope all sessions on this host (default)") || !strings.Contains(out, "scope all sessions on this host") {
		t.Fatalf("explicit scope output %q", out)
	}
}

func TestEnrollScopeFlagsConflict(t *testing.T) {
	h := newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--all-sessions", "--workgroup", "wg1"}, h.env); err == nil || len(h.requests) != 0 {
		t.Fatalf("both scope flags accepted (%v)", err)
	}
}

func TestEnrollDefaultsAreLeastPrivilege(t *testing.T) {
	h := newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--workgroup", "wg1"}, h.env); err != nil {
		t.Fatal(err)
	}
	req := h.requests[0]
	if req.Op != direct.OpEnroll || req.Label != "studio-20261122-1530" || strings.Join(req.Permissions, ",") != "read" ||
		req.Resource.Kind != direct.ResourceWorkgroups || req.Resource.Workgroups[0] != "wg1" {
		t.Fatalf("request %+v", req)
	}
	if !strings.Contains(h.stderr.String(), `Granting: label "studio-20261122-1530" (default) · permissions read (default) · scope workgroup wg1`) {
		t.Fatalf("effective grant not shown: %q", h.stderr.String())
	}
	if strings.TrimSpace(h.stdout.String()) != fakeBundle(t, 1) {
		t.Fatal("stdout is not the v1 bundle alone")
	}
}

func TestDefaultEnrollLabel(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	if got := defaultEnrollLabel(func() (string, error) { return "box.example.com", nil }, now); got != "box-20260102-0304" {
		t.Fatal(got)
	}
	if got := defaultEnrollLabel(func() (string, error) { return "", errors.New("no") }, now); got != "client-20260102-0304" {
		t.Fatal(got)
	}
	if got := defaultEnrollLabel(func() (string, error) { return "a\x07b", nil }, now); got != "ab-20260102-0304" {
		t.Fatal(got)
	}
}

var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// decodeTerminalQR rebuilds the module matrix from half-block output and
// decodes it with an independent QR decoder.
func decodeTerminalQR(t *testing.T, out string, invert bool) (string, [][]bool) {
	t.Helper()
	var rows [][]bool
	for _, line := range strings.Split(out, "\n") {
		line = sgr.ReplaceAllString(line, "")
		rs := []rune(line)
		if len(rs) < 21 || strings.Trim(line, " █▀▄") != "" {
			continue
		}
		top, bottom := make([]bool, len(rs)), make([]bool, len(rs))
		for i, r := range rs {
			ft, fb := r == '█' || r == '▀', r == '█' || r == '▄'
			top[i], bottom[i] = ft != invert, fb != invert
		}
		rows = append(rows, top, bottom)
	}
	if len(rows) == 0 {
		t.Fatalf("no QR rows in output")
	}
	return decodeMatrix(t, rows), rows
}

func decodeMatrix(t *testing.T, m [][]bool) string {
	t.Helper()
	const s = 4
	img := image.NewGray(image.Rect(0, 0, len(m[0])*s, len(m)*s))
	for y := range img.Bounds().Dy() {
		for x := range img.Bounds().Dx() {
			v := uint8(255)
			if m[y/s][x/s] {
				v = 0
			}
			img.Pix[y*img.Stride+x] = v
		}
	}
	return decodeImage(t, img)
}

func decodeImage(t *testing.T, img image.Image) string {
	t.Helper()
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		t.Fatal(err)
	}
	res, err := gozxingqr.NewQRCodeReader().Decode(bmp, map[gozxing.DecodeHintType]interface{}{gozxing.DecodeHintType_PURE_BARCODE: true})
	if err != nil {
		t.Fatalf("decode QR: %v", err)
	}
	return res.GetText()
}

func TestRenderQRDecodesWithQuietZone(t *testing.T) {
	ticket := fakeBundle(t, 2)
	for _, tc := range []struct{ invert, colors bool }{{false, true}, {true, true}, {false, false}, {true, false}} {
		var buf bytes.Buffer
		if err := renderQRHalfBlocks(&buf, ticket, tc.invert, tc.colors); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(buf.String(), "\x1b") != tc.colors {
			t.Errorf("%+v: SGR presence wrong", tc)
		}
		got, rows := decodeTerminalQR(t, buf.String(), tc.invert)
		if got != ticket {
			t.Fatalf("%+v: decoded a different text", tc)
		}
		// A quiet zone of at least 4 light modules on every side.
		n := len(rows[0])
		for i := 0; i < qrQuietZone; i++ {
			for j := 0; j < n; j++ {
				if rows[i][j] || rows[j%len(rows)][i] || rows[j%len(rows)][n-1-i] {
					t.Fatalf("%+v: dark module inside the quiet zone", tc)
				}
			}
		}
	}
}

// The real bundle shape, at the chosen level M, must stay a comfortable scan.
func TestQRSizeBudget(t *testing.T) {
	for _, eps := range [][]string{
		{"https://macbook-pro.tail1a2b3c.ts.net:8443"},
		{"https://192.168.1.20:8443", "https://amux.home.example:8443", "https://macbook-pro.tail1a2b3c.ts.net:8443"},
	} {
		ticket := fakeBundle(t, 2, eps...)
		m, err := qrMatrix(ticket)
		if err != nil {
			t.Fatal(err)
		}
		v := qrVersion(len(m))
		t.Logf("%d endpoint(s): ticket %d bytes -> QR version %d, %d modules (+%d quiet zone each side)", len(eps), len(ticket), v, len(m), qrQuietZone)
		if v > 15 {
			t.Errorf("QR version %d is too dense for a terminal scan", v)
		}
	}
}

func TestQRTooLongErrorDoesNotLeakContent(t *testing.T) {
	secret := strings.Repeat("S3CRET", 1000)
	_, err := qrMatrix(secret)
	if err == nil || strings.Contains(err.Error(), "S3CRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnrollQRMintsOneTicketOnStdoutAndStderr(t *testing.T) {
	h := newEnrollHarness(t)
	h.env.stderrTTY = true
	if err := runDirectEnroll([]string{"--qr", "--all-sessions"}, h.env); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 1 || h.requests[0].Op != direct.OpEnrollTicket {
		t.Fatalf("requests %+v", h.requests)
	}
	ticket := fakeBundle(t, 2)
	if h.stdout.String() != ticket+"\n" {
		t.Fatal("stdout is not the same ticket alone")
	}
	errOut := h.stderr.String()
	if i, j := strings.Index(errOut, "Scan with the Harness Android app"), strings.Index(errOut, "█"); i < 0 || j < i {
		t.Fatal("instructions are not above the QR code")
	}
	if got, _ := decodeTerminalQR(t, errOut, false); got != ticket {
		t.Fatal("the QR code is not the stdout ticket")
	}
	if h.tty.Len() != 0 {
		t.Fatal("wrote to /dev/tty although stderr is a terminal")
	}
}

func TestEnrollQRFallsBackToTTYWhenStderrRedirected(t *testing.T) {
	h := newEnrollHarness(t)
	h.env.getenv = func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}
	if err := runDirectEnroll([]string{"--qr", "--invert", "--all-sessions"}, h.env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.stderr.String(), "█") {
		t.Fatal("QR drawn on a redirected stderr")
	}
	if strings.Contains(h.tty.String(), "\x1b") {
		t.Fatal("colors despite NO_COLOR")
	}
	if got, _ := decodeTerminalQR(t, h.tty.String(), true); got != fakeBundle(t, 2) {
		t.Fatal("tty QR does not decode to the ticket")
	}

	// No terminal anywhere: refused before anything is minted.
	h = newEnrollHarness(t)
	h.env.openTTY = func() (io.WriteCloser, error) { return nil, os.ErrNotExist }
	if err := runDirectEnroll([]string{"--qr", "--all-sessions"}, h.env); err == nil || len(h.requests) != 0 {
		t.Fatalf("err %v, %d requests", err, len(h.requests))
	}
}

func TestEnrollQRPNG(t *testing.T) {
	h := newEnrollHarness(t)
	path := filepath.Join(t.TempDir(), "enroll.png")
	if err := runDirectEnroll([]string{"--qr-png", path, "--all-sessions"}, h.env); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("png mode %v %v", st, err)
	}
	f, _ := os.Open(path)
	img, err := png.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	ticket := fakeBundle(t, 2)
	if decodeImage(t, img) != ticket || h.stdout.String() != ticket+"\n" {
		t.Fatal("PNG and stdout are not the same ticket")
	}
	if h.requests[0].Op != direct.OpEnrollTicket || !strings.Contains(h.stderr.String(), "delete it after enrolling") {
		t.Fatalf("op %s / note missing", h.requests[0].Op)
	}
}

func TestEnrollQRPNGNeverTouchesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taken.png")
	if err := os.WriteFile(path, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, link} {
		h := newEnrollHarness(t)
		if err := runDirectEnroll([]string{"--qr-png", p, "--all-sessions"}, h.env); err == nil || len(h.requests) != 0 {
			t.Fatalf("%s: err %v, %d requests", p, err, len(h.requests))
		}
	}
	st, _ := os.Stat(path)
	data, _ := os.ReadFile(path)
	if st.Mode().Perm() != 0o644 || string(data) != "keep" {
		t.Fatal("an existing file was modified")
	}
}

func TestEnrollQRPNGRemovedWhenMintingFails(t *testing.T) {
	h := newEnrollHarness(t)
	h.err = errors.New("unknown admin operation \"enroll_ticket\"")
	path := filepath.Join(t.TempDir(), "x.png")
	err := runDirectEnroll([]string{"--qr-png", path, "--all-sessions"}, h.env)
	if err == nil || !strings.Contains(err.Error(), "restart it") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty PNG left behind")
	}
}

func TestEnrollQRRefusesAV1Answer(t *testing.T) {
	h := newEnrollHarness(t)
	h.env.stderrTTY = true
	h.resp = direct.AdminResponse{OK: true, Bundle: fakeBundle(t, 1), ExpiresAt: 1_900_000_000}
	if err := runDirectEnroll([]string{"--qr", "--all-sessions"}, h.env); err == nil {
		t.Fatal("drew a v1 bundle as a QR ticket")
	}
	if h.stdout.Len() != 0 || strings.Contains(h.stderr.String(), "█") {
		t.Fatal("printed something for a refused answer")
	}
}
