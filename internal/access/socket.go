package access

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

// ServerTLSConfig returns a fresh config for the daemon's pinned TLS identity.
// The socket path is only routing; all streaming bytes, including client proof,
// flow inside this authenticated encrypted channel.
func (a *FileAuthority) ServerTLSConfig() (*tls.Config, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return nil, ErrAuthorityClosed
	}
	der, err := base64.RawStdEncoding.DecodeString(a.tls.Certificate)
	if err != nil {
		return nil, ErrInvalidCredential
	}
	key, err := base64.RawStdEncoding.DecodeString(a.tls.PrivateKey)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidCredential
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: ed25519.PrivateKey(key)}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLSConfig trusts exactly the daemon certificate carried in protected
// credential material. System roots and caller-controlled server names are not
// consulted, so a hostile socket endpoint cannot solicit or relay a proof.
func ClientTLSConfig(credential Credential) (*tls.Config, error) {
	der, err := base64.RawStdEncoding.DecodeString(credential.DaemonCertificate)
	if err != nil {
		return nil, ErrInvalidCredential
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil || cert.VerifyHostname(daemonTLSServerName) != nil {
		return nil, ErrInvalidCredential
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{
		RootCAs:    pool,
		ServerName: daemonTLSServerName,
		MinVersion: tls.VersionTLS13,
	}, nil
}

const (
	FrameChallenge = "auth.challenge"
	FrameProof     = "auth.proof"
	FrameWelcome   = "auth.welcome"
)

type SocketChallenge struct {
	Type        string `json:"type"`
	Protocol    int    `json:"protocol"`
	BootID      string `json:"bootId"`
	Nonce       string `json:"nonce"`
	IssuerKeyID string `json:"issuerKeyId"`
	Signature   string `json:"signature"`
}

type SocketProof struct {
	Type        string `json:"type"`
	KeyID       string `json:"keyId"`
	Generation  uint64 `json:"generation"`
	ClientNonce string `json:"clientNonce"`
	Signature   string `json:"signature"`
}

type SocketWelcome struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (a *FileAuthority) NewChallenge() (SocketChallenge, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return SocketChallenge{}, ErrAuthorityClosed
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(a.rand, nonce); err != nil {
		return SocketChallenge{}, err
	}
	challenge := SocketChallenge{
		Type: FrameChallenge, Protocol: ProtocolVersion, BootID: a.bootID,
		Nonce: hex.EncodeToString(nonce), IssuerKeyID: a.issuer.KeyID,
	}
	priv, err := base64.RawStdEncoding.DecodeString(a.issuer.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return SocketChallenge{}, ErrInvalidCredential
	}
	challenge.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, challengeBytes(challenge)))
	return challenge, nil
}

func VerifyChallenge(credential Credential, challenge SocketChallenge) error {
	if challenge.Type != FrameChallenge || challenge.Protocol != ProtocolVersion ||
		subtle.ConstantTimeCompare([]byte(credential.IssuerKeyID), []byte(challenge.IssuerKeyID)) != 1 ||
		!validHexID(challenge.BootID, 32) || !validHexID(challenge.Nonce, 32) {
		return ErrInvalidCredential
	}
	pub, err := base64.RawStdEncoding.DecodeString(credential.IssuerPublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrInvalidCredential
	}
	sig, err := base64.RawStdEncoding.DecodeString(challenge.Signature)
	if err != nil || !ed25519.Verify(pub, challengeBytes(challenge), sig) {
		return ErrInvalidCredential
	}
	return nil
}

func SignProof(credential Credential, challenge SocketChallenge, random io.Reader) (SocketProof, error) {
	if err := VerifyChallenge(credential, challenge); err != nil {
		return SocketProof{}, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return SocketProof{}, err
	}
	proof := SocketProof{
		Type: FrameProof, KeyID: credential.KeyID, Generation: credential.Generation,
		ClientNonce: hex.EncodeToString(nonce),
	}
	priv, err := base64.RawStdEncoding.DecodeString(credential.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return SocketProof{}, ErrInvalidCredential
	}
	proof.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, proofBytes(challenge, proof)))
	return proof, nil
}

// VerifyProof authenticates one connection-specific proof. The caller owns the
// challenge and accepts exactly one proof, making its nonce single-use without a
// global replay ledger. Generation/revocation is rechecked by Valid afterward.
func (a *FileAuthority) VerifyProof(ctx context.Context, challenge SocketChallenge, proof SocketProof, allowed ...SubjectKind) (Principal, error) {
	if challenge.BootID != a.bootID || proof.Type != FrameProof || !validHexID(proof.ClientNonce, 16) {
		return Principal{}, ErrInvalidCredential
	}
	a.mu.Lock()
	if a.rootDir == nil {
		a.mu.Unlock()
		return Principal{}, ErrAuthorityClosed
	}
	rec, ok := a.registry.Records[proof.KeyID]
	a.mu.Unlock()
	if !ok || rec.Generation != proof.Generation {
		return Principal{}, ErrInvalidCredential
	}
	permitted := false
	for _, kind := range allowed {
		if rec.Kind == kind {
			permitted = true
			break
		}
	}
	if !permitted {
		return Principal{}, ErrInvalidCredential
	}
	pub, err := base64.RawStdEncoding.DecodeString(rec.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Principal{}, ErrInvalidCredential
	}
	sig, err := base64.RawStdEncoding.DecodeString(proof.Signature)
	if err != nil || !ed25519.Verify(pub, proofBytes(challenge, proof), sig) {
		return Principal{}, ErrInvalidCredential
	}
	p := Principal{KeyID: rec.KeyID, SubjectID: rec.SubjectID, Kind: rec.Kind, Generation: rec.Generation}
	if err := a.Valid(ctx, p); err != nil {
		return Principal{}, err
	}
	return p, nil
}

func challengeBytes(challenge SocketChallenge) []byte {
	return framed("amux-daemon-challenge-v1", challenge.BootID, challenge.Nonce, challenge.IssuerKeyID)
}

func proofBytes(challenge SocketChallenge, proof SocketProof) []byte {
	h := sha256.Sum256(challengeBytes(challenge))
	return framed("amux-daemon-client-proof-v1", hex.EncodeToString(h[:]), proof.KeyID,
		fmt.Sprint(proof.Generation), proof.ClientNonce)
}

func framed(parts ...string) []byte {
	var out []byte
	for _, part := range parts {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		out = append(out, size[:]...)
		out = append(out, part...)
	}
	return out
}
