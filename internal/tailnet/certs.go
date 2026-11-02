package tailnet

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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
// covers host (when host is non-empty) and is valid at now.
func ValidatePair(certPEM, keyPEM []byte, host string, now time.Time) (tls.Certificate, *x509.Certificate, error) {
	return ValidatePairFor(certPEM, keyPEM, hostList(host), now)
}

// ValidatePairFor is ValidatePair for every host in hosts: the leaf must
// cover each one, by DNS name (wildcards included) or IP SAN, as a client
// dialing that host would require. Trust in the issuer is not checked here,
// so a private-CA certificate is accepted; VerifyChain is the separate
// public-root check the managed store applies.
func ValidatePairFor(certPEM, keyPEM []byte, hosts []string, now time.Time) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("certificate and key do not form a pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pair.Leaf = leaf
	if err := Covers(leaf, hosts); err != nil {
		return tls.Certificate{}, nil, err
	}
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, nil, fmt.Errorf("certificate is not valid now (valid %s to %s)", leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return pair, leaf, nil
}

// Covers reports, as an error naming the first gap, whether leaf covers
// every host (DNS name, wildcard, or IP literal).
func Covers(leaf *x509.Certificate, hosts []string) error {
	for _, h := range hosts {
		if err := leaf.VerifyHostname(h); err != nil {
			return fmt.Errorf("certificate (for %s) does not cover %s", strings.Join(certNames(leaf), ", "), h)
		}
	}
	return nil
}

func certNames(leaf *x509.Certificate) []string {
	names := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = append(names, leaf.Subject.CommonName)
	}
	if len(names) == 0 {
		names = []string{"no names"}
	}
	return names
}

func hostList(host string) []string {
	if host == "" {
		return nil
	}
	return []string{host}
}

// LoadPair reads and validates a pair from files.
func LoadPair(certFile, keyFile, host string, now time.Time) (tls.Certificate, PairInfo, error) {
	return LoadPairFor(certFile, keyFile, hostList(host), now)
}

// LoadPairFor reads a pair from files and validates it for every host.
func LoadPairFor(certFile, keyFile string, hosts []string, now time.Time) (tls.Certificate, PairInfo, error) {
	c, err := os.ReadFile(certFile)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, err
	}
	k, err := os.ReadFile(keyFile)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, err
	}
	pair, leaf, err := ValidatePairFor(c, k, hosts, now)
	if err != nil {
		return tls.Certificate{}, PairInfo{}, fmt.Errorf("%s: %w", certFile, err)
	}
	return pair, PairInfo{CertFile: certFile, KeyFile: keyFile, NotAfter: leaf.NotAfter, Issuer: leaf.Issuer.CommonName}, nil
}

// EndpointHosts returns the hosts of https endpoints (deduplicated), the
// names a served certificate must cover.
func EndpointHosts(endpoints []string) []string {
	var hosts []string
	for _, e := range endpoints {
		u, err := url.Parse(e)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if h := u.Hostname(); !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
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
