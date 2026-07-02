package wiretls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// genCert writes a self-signed cert/key valid for 127.0.0.1 + localhost into dir
// and returns their paths. The cert doubles as its own CA (self-signed), so a
// client that trusts certFile verifies the handshake.
func genCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "amux-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// TestHandshake proves a client that trusts the server's CA completes the TLS
// handshake, and one that does not (system roots only) is rejected — the seam
// verifies identity, it doesn't merely encrypt.
func TestHandshake(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := genCert(t, dir)

	srvCfg, err := ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Force the handshake, then drop the connection.
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
			_ = c.Close()
		}
	}()
	addr := ln.Addr().String()

	t.Run("accept with trusted CA", func(t *testing.T) {
		c, err := DialCA("tcp", addr, certFile, "")
		if err != nil {
			t.Fatalf("trusted dial rejected: %v", err)
		}
		_ = c.Close()
	})

	t.Run("reject untrusted cert", func(t *testing.T) {
		// No CA file: only the system roots, which don't include our self-signed
		// cert, so verification must fail.
		if c, err := DialCA("tcp", addr, "", ""); err == nil {
			_ = c.Close()
			t.Fatal("untrusted self-signed cert was accepted; verification is not enforced")
		}
	})
}

func TestServerConfigErrors(t *testing.T) {
	if _, err := ServerConfig("", "", ""); err == nil {
		t.Fatal("expected error when cert/key are missing")
	}
	if _, err := ClientConfig("/no/such/ca.pem", ""); err == nil {
		t.Fatal("expected error for a missing CA file")
	}
}
