package tailnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cert source modes saved in the setting.
const (
	// CertTailscale: amux obtains the certificate from tailscaled and keeps it
	// in its CertStore, renewing it while serving.
	CertTailscale = "tailscale"
	// CertExplicit: the operator supplies the files (--tls-cert/--tls-key);
	// amux reloads them when they change but never writes them.
	CertExplicit = "explicit"
)

// Reloader serves the current certificate to new TLS handshakes. Swapping it
// never touches connections that already completed their handshake, so open
// streams continue on the certificate they negotiated.
type Reloader struct {
	cur atomic.Pointer[tls.Certificate]
}

// NewReloader starts with c.
func NewReloader(c tls.Certificate) *Reloader {
	r := &Reloader{}
	r.cur.Store(&c)
	return r
}

// Set replaces the certificate new handshakes get.
func (r *Reloader) Set(c tls.Certificate) { r.cur.Store(&c) }

// Current is the certificate new handshakes get.
func (r *Reloader) Current() *tls.Certificate { return r.cur.Load() }

// GetCertificate is a tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.cur.Load(), nil
}

// Renewer keeps a Reloader's certificate fresh from inside the process that
// owns the listener. It is not a service of its own: it runs only as long as
// that listener does, and a failed attempt leaves the current certificate in
// place, on disk and in memory.
type Renewer struct {
	Mode   string // CertTailscale or CertExplicit
	Target *Reloader

	// CertTailscale.
	Store CertStore
	Fetch func(ctx context.Context, minValidity time.Duration) (certPEM, keyPEM []byte, err error)

	// CertExplicit. Hosts are the effective endpoint hosts every reloaded
	// pair must cover; a replacement that does not is refused and the
	// current certificate kept.
	CertFile, KeyFile string
	Hosts             []string

	Now         func() time.Time
	Interval    time.Duration // between checks (default 12h)
	RetryAfter  time.Duration // after a failed renewal (default 1h)
	RenewBefore time.Duration // default RenewBefore
	Logf        func(string, ...any)
	// OnChange, when set, is called after every check with the state to
	// publish (for amux direct status).
	OnChange func(CertState)

	mu      sync.Mutex
	stamp   string // explicit mode: size+mtime of the files last loaded
	failed  string // explicit mode: size+mtime of a replacement that was refused
	state   CertState
	nextDue time.Time
}

// CertState is the renewal state amux direct status reports. No key material.
type CertState struct {
	Source      string    `json:"source"`
	NotAfter    time.Time `json:"not_after"`
	Issuer      string    `json:"issuer,omitempty"`
	LastCheck   time.Time `json:"last_check"`
	LastRenewal time.Time `json:"last_renewal,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	NextCheck   time.Time `json:"next_check"`
}

func (r *Renewer) defaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Interval <= 0 {
		r.Interval = 12 * time.Hour
	}
	if r.RetryAfter <= 0 {
		r.RetryAfter = time.Hour
	}
	if r.RenewBefore <= 0 {
		r.RenewBefore = RenewBefore
	}
	if r.Logf == nil {
		r.Logf = func(string, ...any) {}
	}
}

// Prime records the certificate the listener starts with.
func (r *Renewer) Prime(info PairInfo) {
	r.defaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Source, r.state.NotAfter, r.state.Issuer = r.Mode, info.NotAfter, info.Issuer
	if r.Mode == CertExplicit {
		r.stamp = fileStamp(r.CertFile, r.KeyFile)
	}
}

// State is the latest published state.
func (r *Renewer) State() CertState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// Check runs one pass: in tailscale mode it picks up a newer stored version
// (from a later enable) and renews when fewer than RenewBefore remain; in
// explicit mode it reloads the files when they changed. It returns the
// failure, if any, after recording it; the served certificate is unchanged
// on failure.
func (r *Renewer) Check(ctx context.Context) error {
	r.defaults()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	r.state.LastCheck = now
	var err error
	switch r.Mode {
	case CertTailscale:
		err = r.checkStore(ctx, now)
	case CertExplicit:
		err = r.checkFiles(now)
	default:
		err = fmt.Errorf("unknown certificate source %q", r.Mode)
	}
	if err != nil {
		r.state.LastError, r.state.LastErrorAt = err.Error(), now
		r.nextDue = now.Add(r.RetryAfter)
		r.Logf("direct: certificate check failed (still serving the current certificate, valid until %s): %v", r.state.NotAfter.Format(time.RFC3339), err)
	} else {
		r.state.LastError, r.state.LastErrorAt = "", time.Time{}
		r.nextDue = now.Add(r.Interval)
		// Never sleep past the point renewal becomes due.
		if due := r.state.NotAfter.Add(-r.RenewBefore); r.Mode == CertTailscale && due.After(now) && due.Before(r.nextDue) {
			r.nextDue = due
		}
	}
	r.state.NextCheck = r.nextDue
	if r.OnChange != nil {
		r.OnChange(r.state)
	}
	return err
}

func (r *Renewer) checkStore(ctx context.Context, now time.Time) error {
	pair, info, loadErr := r.Store.Load(now)
	if loadErr == nil && !sameLeaf(r.Target.Current(), &pair) {
		r.Target.Set(pair)
		r.state.NotAfter, r.state.Issuer = info.NotAfter, info.Issuer
		r.Logf("direct: serving the stored certificate for %s (valid until %s)", r.Store.Host, info.NotAfter.Format(time.RFC3339))
	}
	if loadErr == nil && info.NotAfter.Sub(now) >= r.RenewBefore {
		return nil
	}
	if r.Fetch == nil {
		return errors.New("no certificate source to renew from")
	}
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	certPEM, keyPEM, err := r.Fetch(fctx, r.RenewBefore)
	cancel()
	if err != nil {
		return fmt.Errorf("renew certificate for %s: %w", r.Store.Host, err)
	}
	info, err = r.Store.Install(certPEM, keyPEM, now)
	if err != nil {
		return fmt.Errorf("install renewed certificate for %s: %w", r.Store.Host, err)
	}
	pair, info, err = r.Store.Load(now)
	if err != nil {
		return err
	}
	r.Target.Set(pair)
	r.state.NotAfter, r.state.Issuer, r.state.LastRenewal = info.NotAfter, info.Issuer, now
	r.Logf("direct: renewed the certificate for %s (valid until %s)", r.Store.Host, info.NotAfter.Format(time.RFC3339))
	return nil
}

func (r *Renewer) checkFiles(now time.Time) error {
	stamp := fileStamp(r.CertFile, r.KeyFile)
	if stamp == r.stamp {
		return nil
	}
	r.failed = stamp
	if len(r.Hosts) == 0 {
		return errors.New("no endpoint host to check the certificate against")
	}
	pair, info, err := LoadPairFor(r.CertFile, r.KeyFile, r.Hosts, now)
	if err != nil {
		// Possibly caught between the operator's two writes, or a pair for
		// the wrong name: keep serving the current one and look again at the
		// next (shortened) check, or as soon as the files change again.
		return fmt.Errorf("replacement %s not loaded (still serving the previous certificate): %w; fix the files to cover %s", r.CertFile, err, strings.Join(r.Hosts, ", "))
	}
	r.Target.Set(pair)
	r.stamp, r.failed = stamp, ""
	r.state.NotAfter, r.state.Issuer, r.state.LastRenewal = info.NotAfter, info.Issuer, now
	r.Logf("direct: reloaded %s (valid until %s)", r.CertFile, info.NotAfter.Format(time.RFC3339))
	return nil
}

// Run checks at once, then whenever the next check is due, until ctx ends.
// The explicit mode also polls the files every minute so a replaced pair is
// picked up promptly.
func (r *Renewer) Run(ctx context.Context) {
	r.defaults()
	_ = r.Check(ctx)
	poll := time.NewTicker(time.Minute)
	defer poll.Stop()
	for {
		r.mu.Lock()
		wait := r.nextDue.Sub(r.Now())
		r.mu.Unlock()
		if wait < time.Second {
			wait = time.Second
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			_ = r.Check(ctx)
		case <-poll.C:
			t.Stop()
			if r.Mode == CertExplicit {
				r.mu.Lock()
				st := fileStamp(r.CertFile, r.KeyFile)
				changed := st != r.stamp && st != r.failed
				r.mu.Unlock()
				if changed {
					_ = r.Check(ctx)
				}
			}
		}
	}
}

func sameLeaf(a, b *tls.Certificate) bool {
	if a == nil || b == nil || len(a.Certificate) == 0 || len(b.Certificate) == 0 {
		return false
	}
	return bytes.Equal(a.Certificate[0], b.Certificate[0])
}

func fileStamp(paths ...string) string {
	var b bytes.Buffer
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", p, st.Size(), st.ModTime().UnixNano())
		} else {
			fmt.Fprintf(&b, "%s:missing;", p)
		}
	}
	return b.String()
}

// State is what the process serving in tailnet mode publishes for status:
// the effective (re-derived) address and endpoint, which may differ from the
// last ones saved, and the certificate's renewal state.
type State struct {
	PID         int       `json:"pid"`
	Managed     bool      `json:"managed"`
	Fingerprint string    `json:"fingerprint,omitempty"` // the managed child's saved-configuration fingerprint
	DNSName     string    `json:"dns_name,omitempty"`
	Listen      string    `json:"listen"`
	Endpoints   []string  `json:"endpoints"`
	ControlURL  string    `json:"control_url,omitempty"`
	Cert        CertState `json:"cert"`
	Drift       string    `json:"drift,omitempty"` // the tailnet identity changed since this listener derived its address
	UpdatedAt   time.Time `json:"updated_at"`
}

// WriteState replaces the state file atomically.
func WriteState(path string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".direct-tailnet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ReadState reads the state file.
func ReadState(path string) (State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var st State
	err = json.Unmarshal(b, &st)
	return st, err
}
