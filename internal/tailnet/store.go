package tailnet

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CertStore keeps the certificate pairs amux manages for one host as
// versions, switched by one atomic pointer:
//
//	<Dir>/v<unix-nanos>/cert.pem   (0644)
//	<Dir>/v<unix-nanos>/key.pem    (0600)
//	<Dir>/current -> v<unix-nanos> (symlink, replaced by rename)
//
// A version is written completely, synced and validated as a pair before
// `current` is pointed at it, so a crash at any moment leaves `current` on a
// complete, valid pair: either the old one or the new one, never a mix. Paths
// through `current` (CertFile/KeyFile) are what the saved setting names; Load
// resolves `current` once and reads both files from that one version.
// Writers serialize on <Dir>/.lock (flock), across processes. Durability
// across power loss holds as far as the filesystem honours fsync; a sync
// error is returned as a failed install rather than ignored.
type CertStore struct {
	Dir  string // e.g. ~/.config/amux/tls/tailnet/<host>
	Host string // the name every stored certificate must cover
	Keep int    // versions kept, including current (default 3)
	// Roots verifies a pair's chain before it is installed: the certificate a
	// tailnet issues (or one reused for the managed store) must chain to a
	// public root so browsers accept it. nil means the system roots. Explicit
	// --tls-cert files (which may use a private CA) never go through a
	// CertStore and are not subject to this.
	Roots *x509.CertPool
}

// StoreDir is the store directory for host under amux's config directory.
func StoreDir(configDir, host string) string {
	return filepath.Join(configDir, "tls", "tailnet", host)
}

// CertFile and KeyFile are the stable paths through the current pointer.
func (s CertStore) CertFile() string { return filepath.Join(s.Dir, "current", "cert.pem") }
func (s CertStore) KeyFile() string  { return filepath.Join(s.Dir, "current", "key.pem") }

// Owns reports whether certFile/keyFile are this store's stable paths.
func (s CertStore) Owns(certFile, keyFile string) bool {
	return filepath.Clean(certFile) == s.CertFile() && filepath.Clean(keyFile) == s.KeyFile()
}

// Load reads the current pair, resolving the pointer once so both files come
// from the same version.
func (s CertStore) Load(now time.Time) (tls.Certificate, PairInfo, error) {
	target, err := os.Readlink(filepath.Join(s.Dir, "current"))
	if err != nil {
		return tls.Certificate{}, PairInfo{}, fmt.Errorf("no managed certificate for %s yet: %w", s.Host, err)
	}
	if filepath.IsAbs(target) || strings.Contains(target, string(filepath.Separator)) || !strings.HasPrefix(target, "v") {
		return tls.Certificate{}, PairInfo{}, fmt.Errorf("managed certificate pointer %q is not a version in %s", target, s.Dir)
	}
	v := filepath.Join(s.Dir, target)
	pair, info, err := LoadPair(filepath.Join(v, "cert.pem"), filepath.Join(v, "key.pem"), s.Host, now)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, err
	}
	info.CertFile, info.KeyFile = s.CertFile(), s.KeyFile()
	return pair, info, nil
}

// Install validates certPEM/keyPEM for Host, writes them as a new version and
// atomically points `current` at it. On any failure the existing pointer and
// versions are untouched. Old versions beyond Keep are removed afterwards.
func (s CertStore) Install(certPEM, keyPEM []byte, now time.Time) (PairInfo, error) {
	pair, leaf, err := ValidatePair(certPEM, keyPEM, s.Host, now)
	if err != nil {
		return PairInfo{}, err
	}
	if err := VerifyChain(pair, s.Host, s.Roots, now); err != nil {
		return PairInfo{}, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return PairInfo{}, err
	}
	// One writer at a time, across processes: naming, the pointer switch and
	// pruning all happen under the store lock. Readers need no lock — the
	// pointer is replaced atomically.
	unlock, err := lockFile(filepath.Join(s.Dir, ".lock"))
	if err != nil {
		return PairInfo{}, fmt.Errorf("lock certificate store: %w", err)
	}
	defer unlock()
	// Versions are named by time and always sort after every existing one
	// (decided under the lock), so a pruned name is never reused and the
	// newest install is the newest version.
	n := now.UnixNano()
	if ents, err := os.ReadDir(s.Dir); err == nil {
		for _, e := range ents {
			if v := versionNum(e.Name()); strings.HasPrefix(e.Name(), "v") && v >= n {
				n = v + 1
			}
		}
	}
	name := "v" + strconv.FormatInt(n, 10)
	tmpDir, err := os.MkdirTemp(s.Dir, ".new-")
	if err != nil {
		return PairInfo{}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	if err := writeSynced(filepath.Join(tmpDir, "key.pem"), keyPEM, 0o600); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	if err := writeSynced(filepath.Join(tmpDir, "cert.pem"), certPEM, 0o644); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	// Re-read what landed on disk: the version must be valid as stored.
	if _, _, err := LoadPair(filepath.Join(tmpDir, "cert.pem"), filepath.Join(tmpDir, "key.pem"), s.Host, now); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	if err := syncDir(tmpDir); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	vdir := filepath.Join(s.Dir, name)
	if err := os.Rename(tmpDir, vdir); err != nil {
		cleanup()
		return PairInfo{}, err
	}
	if err := syncDir(s.Dir); err != nil {
		_ = os.RemoveAll(vdir)
		return PairInfo{}, err
	}
	// The switch: a fresh symlink renamed over `current` is atomic.
	link := filepath.Join(s.Dir, ".current-"+name)
	_ = os.Remove(link)
	if err := os.Symlink(name, link); err != nil {
		return PairInfo{}, err
	}
	if err := os.Rename(link, filepath.Join(s.Dir, "current")); err != nil {
		_ = os.Remove(link)
		return PairInfo{}, err
	}
	// The switch has happened; a failure to sync it is reported (the new pair
	// is already what Load returns, but its durability across a crash is not
	// confirmed), and the caller treats it as a failed install.
	if err := syncDir(s.Dir); err != nil {
		return PairInfo{}, fmt.Errorf("certificate installed but the store directory could not be synced: %w", err)
	}
	s.prune()
	return PairInfo{CertFile: s.CertFile(), KeyFile: s.KeyFile(), NotAfter: leaf.NotAfter, Issuer: leaf.Issuer.CommonName}, nil
}

// Import copies an existing qualifying pair (e.g. a server.crt/server.key
// obtained earlier) into the store as a version. The source files are only
// read, never changed or removed.
func (s CertStore) Import(certFile, keyFile string, now time.Time) (PairInfo, error) {
	c, err := os.ReadFile(certFile)
	if err != nil {
		return PairInfo{}, err
	}
	k, err := os.ReadFile(keyFile)
	if err != nil {
		return PairInfo{}, err
	}
	return s.Install(c, k, now)
}

// prune removes versions beyond Keep, oldest first, never the one `current`
// names when prune runs (re-read here, under the store lock).
func (s CertStore) prune() {
	current, err := os.Readlink(filepath.Join(s.Dir, "current"))
	if err != nil {
		return // no committed pointer: remove nothing
	}
	keep := s.Keep
	if keep <= 0 {
		keep = 3
	}
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	var vs []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
			vs = append(vs, e.Name())
		}
	}
	sort.Slice(vs, func(i, j int) bool { return versionNum(vs[i]) < versionNum(vs[j]) })
	for len(vs) > keep {
		if vs[0] != current {
			_ = os.RemoveAll(filepath.Join(s.Dir, vs[0]))
		}
		vs = vs[1:]
	}
}

func versionNum(v string) int64 {
	n, _ := strconv.ParseInt(strings.TrimPrefix(v, "v"), 10, 64)
	return n
}

func writeSynced(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}

// VerifyChain checks that pair's leaf chains, through the intermediates it
// carries, to roots (nil: the system roots) for host at now.
func VerifyChain(pair tls.Certificate, host string, roots *x509.CertPool, now time.Time) error {
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return fmt.Errorf("certificate chain: %w", err)
		}
		inter.AddCert(c)
	}
	if _, err := pair.Leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter, CurrentTime: now}); err != nil {
		return fmt.Errorf("certificate for %s does not chain to a trusted root: %w", host, err)
	}
	return nil
}
