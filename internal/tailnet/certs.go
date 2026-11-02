package tailnet

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RenewBefore is how much validity a certificate must have left to be reused;
// with less, amux asks tailscaled for a fresh one (renewing while serving).
const RenewBefore = 30 * 24 * time.Hour

// PairInfo describes a validated certificate/key pair. It never carries key
// material.
type PairInfo struct {
	CertFile string
	KeyFile  string
	NotAfter time.Time
	Issuer   string
}

// ValidatePair checks that certPEM/keyPEM form a matching pair whose leaf
// covers host and is valid at now. It returns the parsed certificate.
func ValidatePair(certPEM, keyPEM []byte, host string, now time.Time) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate and key do not form a pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair.Leaf = leaf
	if host != "" {
		if err := leaf.VerifyHostname(host); err != nil {
			return tls.Certificate{}, nil, fmt.Errorf("certificate does not cover %s", host)
		}
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, nil, fmt.Errorf("certificate is not valid now (valid %s to %s)", leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return pair, leaf, nil
}

// LoadPair reads and validates a pair from files.
func LoadPair(certFile, keyFile, host string, now time.Time) (tls.Certificate, PairInfo, error) {
	c, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, err
	}
	k, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, err
	}
	pair, leaf, err := ValidatePair(c, k, host, now)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, fmt.Errorf("%s: %w", certFile, err)
	}
	return pair, PairInfo{CertFile: certFile, KeyFile: keyFile, NotAfter: leaf.NotAfter, Issuer: leaf.Issuer.CommonName}, nil
}

// FindReusable looks in dir for an existing pair — <name>.crt beside
// <name>.key, e.g. server.crt/server.key or <host>.crt/<host>.key — that
// covers host and stays valid for at least minLeft. It prefers the one that
// lasts longest. Nothing is modified; a pair that does not qualify is left as
// it is.
func FindReusable(dir, host string, minLeft time.Duration, now time.Time) (PairInfo, bool) {
	certs, _ := filepath.Glob(filepath.Join(dir, "*.crt"))
	sort.Strings(certs)
	var best PairInfo
	for _, c := range certs {
		k := strings.TrimSuffix(c, ".crt") + ".key"
		if _, err := os.Stat(k); err != nil {
			continue
		}
		_, info, err := LoadPair(c, k, host, now)
		if err != nil || info.NotAfter.Sub(now) < minLeft {
			continue
		}
		if info.NotAfter.After(best.NotAfter) {
			best = info
		}
	}
	return best, best.CertFile != ""
}
