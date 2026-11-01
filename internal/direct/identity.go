package direct

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Identity is the direct server's destination identity: a dedicated P-256 key,
// distinct from the local daemon's TLS and issuer keys. ServerID is its RFC 7638
// SHA-256 JWK thumbprint. Generation changes on reset and invalidates grants.
type Identity struct {
	key        *ecdsa.PrivateKey
	ServerID   string
	Generation string
}

type identityFile struct {
	V          int             `json:"v"`
	Generation string          `json:"generation"`
	Key        jose.JSONWebKey `json:"key"`
}

func newIdentity() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	gen := make([]byte, 16)
	if _, err := rand.Read(gen); err != nil {
		return nil, err
	}
	return identityFromKey(key, hex.EncodeToString(gen))
}

func identityFromKey(key *ecdsa.PrivateKey, generation string) (*Identity, error) {
	id, err := Thumbprint(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &Identity{key: key, ServerID: id, Generation: generation}, nil
}

// Thumbprint is the RFC 7638 SHA-256 thumbprint of a P-256 public key, base64url.
func Thumbprint(pub *ecdsa.PublicKey) (string, error) {
	jwk := jose.JSONWebKey{Key: pub}
	sum, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

func (id *Identity) marshal() ([]byte, error) {
	return json.MarshalIndent(identityFile{V: 1, Generation: id.Generation, Key: jose.JSONWebKey{Key: id.key, Algorithm: string(jose.ES256), Use: "sig"}}, "", "  ")
}

func unmarshalIdentity(data []byte) (*Identity, error) {
	var f identityFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("direct identity: %w", err)
	}
	if f.V != 1 {
		return nil, fmt.Errorf("direct identity: unsupported version %d", f.V)
	}
	key, ok := f.Key.Key.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() || f.Generation == "" {
		return nil, errors.New("direct identity: not a P-256 private key")
	}
	return identityFromKey(key, f.Generation)
}

// SignHello answers one hello. The caller has already checked protocol, nonce
// shape and that endpoint is the configured origin the request arrived on.
func (id *Identity) SignHello(req HelloRequest, now time.Time) (string, error) {
	claims := HelloClaims{
		ServerID: id.ServerID, Protocol: req.Protocol, Endpoint: req.Endpoint,
		ClientNonce: req.ClientNonce, IssuedAt: now.Unix(), ExpiresAt: now.Unix() + HelloLifetimeSeconds,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	opts := (&jose.SignerOptions{EmbedJWK: true}).WithType(HelloType)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: id.key}, opts)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

// VerifyHello is the client-side check every direct client must perform before
// it sends a code or credential. It accepts only ES256 with an embedded P-256
// public JWK whose thumbprint is wantServerID, and requires the nonce, endpoint
// and protocol it sent, plus iat/exp within the bounded lifetime and skew. Web
// and Android implement the same checks with their own JOSE libraries; this Go
// implementation is the reference and is exercised against the fixtures.
func VerifyHello(proof, wantServerID string, sent HelloRequest, now time.Time) (HelloClaims, error) {
	obj, err := jose.ParseSigned(proof, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		return HelloClaims{}, fmt.Errorf("hello: %w", err)
	}
	if len(obj.Signatures) != 1 {
		return HelloClaims{}, errors.New("hello: exactly one signature required")
	}
	h := obj.Signatures[0].Header
	if h.Algorithm != string(jose.ES256) {
		return HelloClaims{}, errors.New("hello: algorithm must be ES256")
	}
	if typ, _ := h.ExtraHeaders[jose.HeaderType].(string); typ != HelloType {
		return HelloClaims{}, errors.New("hello: wrong typ")
	}
	jwk := h.JSONWebKey
	if jwk == nil || !jwk.Valid() || !jwk.IsPublic() {
		return HelloClaims{}, errors.New("hello: missing or private jwk")
	}
	pub, ok := jwk.Key.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return HelloClaims{}, errors.New("hello: jwk must be EC P-256")
	}
	got, err := Thumbprint(pub)
	if err != nil || got != wantServerID {
		return HelloClaims{}, errors.New("hello: server identity mismatch")
	}
	payload, err := obj.Verify(pub)
	if err != nil {
		return HelloClaims{}, errors.New("hello: bad signature")
	}
	var c HelloClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return HelloClaims{}, fmt.Errorf("hello: claims: %w", err)
	}
	switch {
	case c.ServerID != wantServerID:
		return HelloClaims{}, errors.New("hello: server_id mismatch")
	case c.Protocol != Protocol || sent.Protocol != Protocol:
		return HelloClaims{}, errors.New("hello: protocol mismatch")
	case c.ClientNonce != sent.ClientNonce:
		return HelloClaims{}, errors.New("hello: nonce mismatch")
	case c.Endpoint != sent.Endpoint:
		return HelloClaims{}, errors.New("hello: endpoint mismatch")
	case c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > HelloLifetimeSeconds:
		return HelloClaims{}, errors.New("hello: lifetime out of bounds")
	case abs(now.Unix()-c.IssuedAt) > HelloMaxSkewSeconds:
		return HelloClaims{}, errors.New("hello: iat outside allowed skew")
	case now.Unix() >= c.ExpiresAt+HelloMaxSkewSeconds:
		return HelloClaims{}, errors.New("hello: expired")
	}
	return c, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// validNonce checks a base64url (no padding) encoding of exactly NonceBytes.
func validNonce(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == NonceBytes && base64.RawURLEncoding.EncodeToString(b) == s
}
