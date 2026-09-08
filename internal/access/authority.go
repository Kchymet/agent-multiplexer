// Package access owns authentication identities and authorization policy for
// daemon clients. Transport reachability is deliberately not identity: every
// socket connection and filesystem-mailbox request has to prove possession of
// a server-issued key before any protected data is returned.
package access

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"amux/internal/core"
)

const (
	ProtocolVersion  = 1
	MaxBodyBytes     = 64 << 10
	maxStateBytes    = 4 << 20
	maxReplayEntries = 4096
	MaxRequestAge    = 30 * time.Second
	clockSkew        = 5 * time.Second
)

type SubjectKind string

const (
	SubjectHost     SubjectKind = "host"
	SubjectProvider SubjectKind = "provider"
	SubjectSession  SubjectKind = "session"
)

var (
	ErrInvalidCredential = errors.New("invalid credential")
	ErrRevoked           = errors.New("credential revoked")
	ErrExpired           = errors.New("request expired")
	ErrReplay            = errors.New("request replayed")
	ErrCapacity          = errors.New("access state capacity exceeded")
	ErrAuthorityInUse    = errors.New("access authority already open")
	ErrAuthorityClosed   = errors.New("access authority is closed")
	ErrNotProvisioned    = errors.New("credential not provisioned")
	ErrGenerationChanged = errors.New("credential generation changed")
)

// CredentialRecord is the daemon-owned public half of an issued credential.
// Role and scope are intentionally absent: policy derives them from the live
// session store for SubjectID, never from credential or request claims.
type CredentialRecord struct {
	KeyID      string      `json:"keyId"`
	SubjectID  string      `json:"subjectId"`
	Kind       SubjectKind `json:"kind"`
	Generation uint64      `json:"generation"`
	PublicKey  string      `json:"publicKey"`
	IssuedAt   int64       `json:"issuedAt"`
	NotAfter   int64       `json:"notAfter"`
	RevokedAt  int64       `json:"revokedAt,omitempty"`
}

type Principal struct {
	KeyID      string
	SubjectID  string
	Kind       SubjectKind
	Generation uint64
}

// Credential is the client-readable private half. It lives only in a stable,
// read-only directory bind dedicated to its subject. The directory is stable so
// replacing current during rotation is visible to an already-running mount
// namespace; bind-mounting this file directly would pin an old inode.
type Credential struct {
	Protocol        int         `json:"protocol"`
	KeyID           string      `json:"keyId"`
	SubjectID       string      `json:"subjectId"`
	Kind            SubjectKind `json:"kind"`
	Generation      uint64      `json:"generation"`
	PrivateKey      string      `json:"privateKey"`
	IssuerKeyID     string      `json:"issuerKeyId"`
	IssuerPublicKey string      `json:"issuerPublicKey"`
	// DaemonCertificate is the DER-encoded TLS trust anchor for the streaming
	// daemon socket. It is public pin material; the matching private key never
	// leaves the daemon-owned authority root.
	DaemonCertificate string `json:"daemonCertificate"`
	NotAfter          int64  `json:"notAfter"`
}

// SignedRequest is the transport-independent authenticated envelope used by
// regular-file mailbox RPC. Body is signed byte-for-byte (through its digest),
// then decoded only after verification.
type SignedRequest struct {
	Protocol   int    `json:"protocol"`
	BootID     string `json:"bootId"`
	KeyID      string `json:"keyId"`
	Generation uint64 `json:"generation"`
	RequestID  string `json:"requestId"`
	IssuedAt   int64  `json:"issuedAt"`
	ExpiresAt  int64  `json:"expiresAt"`
	Body       []byte `json:"body"` // base64 on the wire; exact signed bytes survive JSON encoding
	Signature  string `json:"signature"`
}

type registryFile struct {
	Version int                         `json:"version"`
	Current map[string]string           `json:"current"`
	Records map[string]CredentialRecord `json:"records"`
}

type replayRecord struct {
	KeyID      string `json:"keyId"`
	Generation uint64 `json:"generation"`
	RequestID  string `json:"requestId"`
	BodyDigest string `json:"bodyDigest"`
	ExpiresAt  int64  `json:"expiresAt"`
}

type replayFile struct {
	Version int                     `json:"version"`
	Entries map[string]replayRecord `json:"entries"`
}

// FileAuthority keeps integrity/replay state under a daemon-private root. The
// private keys are stored in subject-specific credential directories beneath
// that root and only those individual directories are mounted into sandboxes.
type FileAuthority struct {
	root     string
	now      func() time.Time
	rand     io.Reader
	bootID   string
	issuer   issuerCredential
	tls      daemonTLSCredential
	commit   func(string, any, os.FileMode) (bool, error)
	syncFile func(*os.File) error
	lock     *os.File
	rootDir  *os.File

	mu       sync.RWMutex
	registry registryFile
	replay   replayFile
	watchers map[string]map[chan struct{}]struct{}
}

type issuerCredential struct {
	KeyID      string `json:"keyId"`
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"`
}

type daemonTLSCredential struct {
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

const LocalHostSubject = "local-operator"

func AuthorityRoot() string { return filepath.Join(core.StateDir(), "access", "v1") }

func OpenDefault() (*FileAuthority, error) { return Open(AuthorityRoot()) }

func Open(root string) (*FileAuthority, error) {
	return open(root, time.Now, rand.Reader)
}

func open(root string, now func() time.Time, random io.Reader) (*FileAuthority, error) {
	if root == "" {
		return nil, fmt.Errorf("access authority root is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create access authority: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure access authority: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(root, ".authority.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open access authority lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAuthorityInUse
		}
		return nil, fmt.Errorf("lock access authority: %w", err)
	}
	rootDir, err := os.Open(root)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, fmt.Errorf("open access authority root: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = rootDir.Close()
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
		}
	}()
	a := &FileAuthority{
		root: root, now: now, rand: random,
		registry: registryFile{Version: ProtocolVersion, Current: map[string]string{}, Records: map[string]CredentialRecord{}},
		replay:   replayFile{Version: ProtocolVersion, Entries: map[string]replayRecord{}},
		commit:   atomicJSONCommit,
		syncFile: func(file *os.File) error { return file.Sync() },
		lock:     lock,
		rootDir:  rootDir,
		watchers: make(map[string]map[chan struct{}]struct{}),
	}
	if err := a.loadOrCreateIssuer(); err != nil {
		return nil, err
	}
	if err := a.loadOrCreateDaemonTLS(); err != nil {
		return nil, err
	}
	if err := readJSON(filepath.Join(root, "registry.json"), &a.registry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read access registry: %w", err)
	}
	if err := readJSON(filepath.Join(root, "replay.json"), &a.replay); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read replay ledger: %w", err)
	}
	if a.registry.Current == nil {
		a.registry.Current = map[string]string{}
	}
	if a.registry.Records == nil {
		a.registry.Records = map[string]CredentialRecord{}
	}
	if a.replay.Entries == nil {
		a.replay.Entries = map[string]replayRecord{}
	}
	boot := make([]byte, 32)
	if _, err := io.ReadFull(random, boot); err != nil {
		return nil, fmt.Errorf("create daemon boot nonce: %w", err)
	}
	a.bootID = hex.EncodeToString(boot)
	ok = true
	return a, nil
}

// Close releases this root's exclusive authority ownership. A daemon must own
// exactly one FileAuthority for its lifetime; opening a second object would
// otherwise retain stale registry/revocation memory.
func (a *FileAuthority) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lock == nil {
		return nil
	}
	for keyID := range a.watchers {
		a.invalidateLocked(keyID)
	}
	rootErr := a.rootDir.Close()
	a.rootDir = nil
	err := syscall.Flock(int(a.lock.Fd()), syscall.LOCK_UN)
	closeErr := a.lock.Close()
	a.lock = nil
	if err != nil {
		return err
	}
	if rootErr != nil {
		return rootErr
	}
	return closeErr
}

func (a *FileAuthority) Root() string   { return a.root }
func (a *FileAuthority) BootID() string { return a.bootID }

// IssuerKeyID identifies the daemon authority signer without exposing its
// private key. An empty value means the authority has been closed.
func (a *FileAuthority) IssuerKeyID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return ""
	}
	return a.issuer.KeyID
}

// SignIssuer signs exact, caller-domain-separated service/response bytes for
// filesystem RPC. The issuer private key never leaves the authority.
func (a *FileAuthority) SignIssuer(message []byte) ([]byte, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return nil, ErrAuthorityClosed
	}
	priv, err := base64.RawStdEncoding.DecodeString(a.issuer.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return nil, ErrInvalidCredential
	}
	return ed25519.Sign(ed25519.PrivateKey(priv), message), nil
}

func subjectKey(kind SubjectKind, id string) string {
	d := sha256.Sum256([]byte(string(kind) + "\x00" + id))
	return hex.EncodeToString(d[:16])
}

func currentKey(kind SubjectKind, id string) string { return string(kind) + "\x00" + id }

func (a *FileAuthority) CredentialDir(kind SubjectKind, subjectID string) string {
	return filepath.Join(a.root, "credentials", subjectKey(kind, subjectID))
}

func DefaultCredentialDir(kind SubjectKind, subjectID string) string {
	return filepath.Join(AuthorityRoot(), "credentials", subjectKey(kind, subjectID))
}

// ProtectedHostCredentialDir is independent of caller-controlled HOME/XDG and
// is used only as the capability gate for automatic host daemon startup.
func ProtectedHostCredentialDir() (string, error) {
	stateDir, err := core.ProtectedStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "access", "v1", "credentials", subjectKey(SubjectHost, LocalHostSubject)), nil
}

func (a *FileAuthority) EnsureHost(ctx context.Context) (string, error) {
	return a.Ensure(ctx, SubjectHost, LocalHostSubject)
}

// Ensure returns the current credential directory for a subject, provisioning
// generation one only when the subject has never been issued. Revocation and
// expiry are sticky and require an explicit higher-level recovery decision. It
// never returns private key bytes.
func (a *FileAuthority) Ensure(_ context.Context, kind SubjectKind, subjectID string) (string, error) {
	if err := validateSubject(kind, subjectID); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return "", ErrAuthorityClosed
	}
	return a.ensureLocked(kind, subjectID)
}

func (a *FileAuthority) ensureLocked(kind SubjectKind, subjectID string) (string, error) {
	keyID := a.registry.Current[currentKey(kind, subjectID)]
	if keyID == "" {
		if a.subjectWasIssuedLocked(kind, subjectID) {
			return "", ErrRevoked
		}
		return a.issueLocked(kind, subjectID, "")
	}
	rec, err := a.currentRecordLocked(kind, subjectID)
	if err != nil {
		return "", err
	}
	if err := a.ensureCredentialPublished(rec); err != nil {
		return "", err
	}
	return a.CredentialDir(kind, subjectID), nil
}

// Current returns the committed current generation without ever issuing a new
// credential. If the registry commit succeeded but publication of current did
// not, it completes that staged publication while holding authority ownership.
// This lets lifecycle maintenance recover a crash window without treating a
// revoked subject as a never-provisioned subject.
func (a *FileAuthority) Current(_ context.Context, kind SubjectKind, subjectID string) (CredentialRecord, error) {
	if err := validateSubject(kind, subjectID); err != nil {
		return CredentialRecord{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return CredentialRecord{}, ErrAuthorityClosed
	}
	rec, err := a.currentRecordLocked(kind, subjectID)
	if err != nil {
		return CredentialRecord{}, err
	}
	if err := a.ensureCredentialPublished(rec); err != nil {
		return CredentialRecord{}, err
	}
	return rec, nil
}

// LastRevoked returns the authoritative latest revoked generation for an
// explicitly authorized restore transition. It neither publishes nor issues a
// credential. Callers pass its Generation to Regrant only after quiescing stale
// completion ownership. They may keep the daemon-owned subject archived while
// publishing the successor, then commit unarchive after publication succeeds.
func (a *FileAuthority) LastRevoked(_ context.Context, kind SubjectKind, subjectID string) (CredentialRecord, error) {
	if err := validateSubject(kind, subjectID); err != nil {
		return CredentialRecord{}, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return CredentialRecord{}, ErrAuthorityClosed
	}
	if a.registry.Current[currentKey(kind, subjectID)] != "" {
		return CredentialRecord{}, ErrGenerationChanged
	}
	rec, err := a.lastRecordLocked(kind, subjectID)
	if err != nil {
		return CredentialRecord{}, err
	}
	if rec.RevokedAt == 0 {
		return CredentialRecord{}, ErrInvalidCredential
	}
	return rec, nil
}

// Regrant explicitly restores a revoked subject at the generation immediately
// following expectedRevokedGeneration. It is not an automatic renewal API:
// callers must first authorize the restore and invalidate stale completion
// ownership. They may retain archived policy until successor publication is
// confirmed. A current, missing, non-latest, or differently generated
// predecessor fails closed.
func (a *FileAuthority) Regrant(_ context.Context, kind SubjectKind, subjectID string, expectedRevokedGeneration uint64) (string, error) {
	if err := validateSubject(kind, subjectID); err != nil {
		return "", err
	}
	if expectedRevokedGeneration == 0 {
		return "", ErrGenerationChanged
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return "", ErrAuthorityClosed
	}
	if a.registry.Current[currentKey(kind, subjectID)] != "" {
		return "", ErrGenerationChanged
	}
	rec, err := a.lastRecordLocked(kind, subjectID)
	if err != nil {
		return "", err
	}
	if rec.RevokedAt == 0 || rec.Generation != expectedRevokedGeneration {
		return "", ErrGenerationChanged
	}
	return a.issueLocked(kind, subjectID, rec.KeyID)
}

func (a *FileAuthority) currentRecordLocked(kind SubjectKind, subjectID string) (CredentialRecord, error) {
	keyID := a.registry.Current[currentKey(kind, subjectID)]
	if keyID == "" {
		if a.subjectWasIssuedLocked(kind, subjectID) {
			return CredentialRecord{}, ErrRevoked
		}
		return CredentialRecord{}, ErrNotProvisioned
	}
	rec, ok := a.registry.Records[keyID]
	if !ok || rec.KeyID != keyID || rec.Kind != kind || rec.SubjectID != subjectID {
		return CredentialRecord{}, ErrInvalidCredential
	}
	if rec.RevokedAt != 0 {
		return CredentialRecord{}, ErrRevoked
	}
	if rec.NotAfter <= a.now().UnixMilli() {
		return CredentialRecord{}, ErrExpired
	}
	return rec, nil
}

func (a *FileAuthority) subjectWasIssuedLocked(kind SubjectKind, subjectID string) bool {
	for _, rec := range a.registry.Records {
		if rec.Kind == kind && rec.SubjectID == subjectID {
			return true
		}
	}
	return false
}

func (a *FileAuthority) lastRecordLocked(kind SubjectKind, subjectID string) (CredentialRecord, error) {
	var last CredentialRecord
	found := false
	for keyID, rec := range a.registry.Records {
		if rec.Kind != kind || rec.SubjectID != subjectID {
			continue
		}
		if rec.KeyID != keyID {
			return CredentialRecord{}, ErrInvalidCredential
		}
		if !found || rec.Generation > last.Generation {
			last = rec
			found = true
			continue
		}
		if rec.Generation == last.Generation && rec.KeyID != last.KeyID {
			return CredentialRecord{}, ErrInvalidCredential
		}
	}
	if !found {
		return CredentialRecord{}, ErrNotProvisioned
	}
	return last, nil
}

// Rotate invalidates the current generation before publishing a replacement.
// A failure during publication is fail-closed: the old key remains revoked.
func (a *FileAuthority) Rotate(_ context.Context, kind SubjectKind, subjectID string) (string, error) {
	if err := validateSubject(kind, subjectID); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return "", ErrAuthorityClosed
	}
	rec, err := a.currentRecordLocked(kind, subjectID)
	if err != nil {
		return "", err
	}
	return a.issueLocked(kind, subjectID, rec.KeyID)
}

func (a *FileAuthority) issueLocked(kind SubjectKind, subjectID, oldKeyID string) (string, error) {
	now := a.now()
	current := currentKey(kind, subjectID)
	nextRegistry := cloneRegistry(a.registry)
	currentKeyID := nextRegistry.Current[current]
	if (oldKeyID == "" && currentKeyID != "") ||
		(oldKeyID != "" && currentKeyID != "" && currentKeyID != oldKeyID) {
		return "", ErrGenerationChanged
	}
	generation := uint64(1)
	if oldID := oldKeyID; oldID != "" {
		old, ok := nextRegistry.Records[oldID]
		if !ok || old.Kind != kind || old.SubjectID != subjectID || old.Generation == ^uint64(0) {
			return "", ErrInvalidCredential
		}
		generation = old.Generation + 1
		if old.RevokedAt == 0 {
			old.RevokedAt = now.UnixMilli()
			nextRegistry.Records[oldID] = old
		}
	}
	pub, priv, err := ed25519.GenerateKey(a.rand)
	if err != nil {
		return "", fmt.Errorf("generate access credential: %w", err)
	}
	digest := sha256.Sum256(pub)
	keyID := hex.EncodeToString(digest[:16])
	notAfter := now.Add(30 * 24 * time.Hour)
	rec := CredentialRecord{
		KeyID: keyID, SubjectID: subjectID, Kind: kind, Generation: generation,
		PublicKey: base64.RawStdEncoding.EncodeToString(pub),
		IssuedAt:  now.UnixMilli(), NotAfter: notAfter.UnixMilli(),
	}
	cred := Credential{
		Protocol: ProtocolVersion, KeyID: keyID, SubjectID: subjectID, Kind: kind, Generation: generation,
		PrivateKey: base64.RawStdEncoding.EncodeToString(priv), IssuerKeyID: a.issuer.KeyID,
		IssuerPublicKey: a.issuer.PublicKey, DaemonCertificate: a.tls.Certificate,
		NotAfter: notAfter.UnixMilli(),
	}
	dir, dirHandle, err := a.ensureCredentialDirLocked(kind, subjectID)
	if err != nil {
		return "", err
	}
	defer dirHandle.Close()
	// Stage the key in the stable directory. It is not authoritative until the
	// registry commit below; a crash here leaves only an unusable staged key.
	staged := stagedCredentialPath(dir, keyID)
	if err := writeJSONFile(staged, cred, 0o400); err != nil {
		return "", err
	}
	// The registry must never durably point at a credential whose staged name or
	// newly-created ancestry has not crossed a directory durability barrier.
	if err := a.syncFile(dirHandle); err != nil {
		_ = os.Remove(staged)
		return "", fmt.Errorf("sync staged credential directory: %w", err)
	}
	nextRegistry.Records[keyID] = rec
	nextRegistry.Current[current] = keyID
	if err := stateFits(nextRegistry); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	committed, err := a.commit(filepath.Join(a.root, "registry.json"), nextRegistry, 0o600)
	if committed {
		a.registry = nextRegistry
		if oldKeyID != "" {
			a.invalidateLocked(oldKeyID)
		}
	}
	if err != nil {
		if !committed {
			_ = os.Remove(staged)
		}
		return "", err
	}
	if err := os.Rename(staged, filepath.Join(dir, "current")); err != nil {
		return "", fmt.Errorf("publish access credential: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (a *FileAuthority) ensureCredentialDirLocked(kind SubjectKind, subjectID string) (string, *os.File, error) {
	credentials, err := ensureDirAt(int(a.rootDir.Fd()), "credentials", 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("open credential authority root: %w", err)
	}
	defer credentials.Close()
	if err := a.syncFile(a.rootDir); err != nil {
		return "", nil, fmt.Errorf("sync credential authority ancestry: %w", err)
	}
	opaque := subjectKey(kind, subjectID)
	subject, err := ensureDirAt(int(credentials.Fd()), opaque, 0o700)
	if err != nil {
		return "", nil, fmt.Errorf("open subject credential directory: %w", err)
	}
	if err := a.syncFile(credentials); err != nil {
		subject.Close()
		return "", nil, fmt.Errorf("sync subject credential ancestry: %w", err)
	}
	if err := a.syncFile(subject); err != nil {
		subject.Close()
		return "", nil, fmt.Errorf("sync subject credential directory: %w", err)
	}
	return a.CredentialDir(kind, subjectID), subject, nil
}

func (a *FileAuthority) ensureCredentialPublished(rec CredentialRecord) error {
	dir := a.CredentialDir(rec.Kind, rec.SubjectID)
	var validationErr error
	for _, name := range []string{"current", filepath.Base(stagedCredentialPath(dir, rec.KeyID))} {
		var cred Credential
		path := filepath.Join(dir, name)
		if err := readJSON(path, &cred); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				validationErr = ErrInvalidCredential
			}
			continue
		}
		if err := a.validatePublishedCredential(rec, cred); err != nil {
			validationErr = err
			continue
		}
		if cred.DaemonCertificate != a.tls.Certificate {
			cred.DaemonCertificate = a.tls.Certificate
			if err := atomicJSON(path, cred, 0o400); err != nil {
				return fmt.Errorf("update access credential trust anchor: %w", err)
			}
		}
		if name != "current" {
			if err := os.Rename(path, filepath.Join(dir, "current")); err != nil {
				return fmt.Errorf("recover staged access credential: %w", err)
			}
			return syncDir(dir)
		}
		return nil
	}
	if validationErr != nil {
		return fmt.Errorf("validate published access credential %s: %w", rec.KeyID, validationErr)
	}
	return fmt.Errorf("current access credential %s is not published", rec.KeyID)
}

func (a *FileAuthority) validatePublishedCredential(rec CredentialRecord, cred Credential) error {
	if cred.Protocol != ProtocolVersion || cred.KeyID != rec.KeyID || cred.SubjectID != rec.SubjectID ||
		cred.Kind != rec.Kind || cred.Generation != rec.Generation || cred.NotAfter != rec.NotAfter ||
		cred.IssuerKeyID != a.issuer.KeyID || cred.IssuerPublicKey != a.issuer.PublicKey {
		return ErrInvalidCredential
	}
	pub, err := base64.RawStdEncoding.DecodeString(rec.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return ErrInvalidCredential
	}
	digest := sha256.Sum256(pub)
	if subtle.ConstantTimeCompare([]byte(rec.KeyID), []byte(hex.EncodeToString(digest[:16]))) != 1 {
		return ErrInvalidCredential
	}
	priv, err := base64.RawStdEncoding.DecodeString(cred.PrivateKey)
	if err != nil || !privateKeyMatchesPublic(priv, pub) {
		return ErrInvalidCredential
	}
	return nil
}

func stagedCredentialPath(dir, keyID string) string {
	return filepath.Join(dir, ".current."+keyID+".next")
}

func cloneRegistry(in registryFile) registryFile {
	out := registryFile{Version: in.Version, Current: make(map[string]string, len(in.Current)), Records: make(map[string]CredentialRecord, len(in.Records))}
	for key, value := range in.Current {
		out.Current[key] = value
	}
	for key, value := range in.Records {
		out.Records[key] = value
	}
	return out
}

func (a *FileAuthority) loadOrCreateIssuer() error {
	path := filepath.Join(a.root, "issuer.json")
	if err := readJSON(path, &a.issuer); err == nil {
		if err := validateIssuerCredential(a.issuer); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read access issuer: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(a.rand)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(pub)
	a.issuer = issuerCredential{
		KeyID: hex.EncodeToString(digest[:16]), PublicKey: base64.RawStdEncoding.EncodeToString(pub),
		PrivateKey: base64.RawStdEncoding.EncodeToString(priv),
	}
	return atomicJSON(path, a.issuer, 0o600)
}

func validateIssuerCredential(issuer issuerCredential) error {
	pub, pubErr := base64.RawStdEncoding.DecodeString(issuer.PublicKey)
	priv, privErr := base64.RawStdEncoding.DecodeString(issuer.PrivateKey)
	if pubErr != nil || privErr != nil || !privateKeyMatchesPublic(priv, pub) {
		return fmt.Errorf("invalid access issuer key")
	}
	digest := sha256.Sum256(pub)
	if subtle.ConstantTimeCompare([]byte(issuer.KeyID), []byte(hex.EncodeToString(digest[:16]))) != 1 {
		return fmt.Errorf("invalid access issuer key id")
	}
	return nil
}

// An Ed25519 private key stores the seed followed by a cached public-key
// suffix. PrivateKey.Public returns that suffix without deriving it from the
// seed, so validate both the complete canonical expansion and the expected
// public key before accepting persisted signing material.
func privateKeyMatchesPublic(private, public []byte) bool {
	if len(private) != ed25519.PrivateKeySize || len(public) != ed25519.PublicKeySize {
		return false
	}
	derived := ed25519.NewKeyFromSeed(private[:ed25519.SeedSize])
	return subtle.ConstantTimeCompare(private, derived) == 1 &&
		subtle.ConstantTimeCompare(public, derived[ed25519.SeedSize:]) == 1
}

const daemonTLSServerName = "amux-daemon"

func (a *FileAuthority) loadOrCreateDaemonTLS() error {
	path := filepath.Join(a.root, "daemon-tls.json")
	if err := readJSON(path, &a.tls); err == nil {
		return validateDaemonTLS(a.tls)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read daemon TLS identity: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(a.rand)
	if err != nil {
		return fmt.Errorf("generate daemon TLS identity: %w", err)
	}
	serialBytes := make([]byte, 16)
	if _, err := io.ReadFull(a.rand, serialBytes); err != nil {
		return fmt.Errorf("generate daemon TLS serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(serialBytes),
		Subject:      pkix.Name{CommonName: daemonTLSServerName},
		DNSNames:     []string{daemonTLSServerName},
		NotBefore:    now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(a.rand, tmpl, tmpl, pub, priv)
	if err != nil {
		return fmt.Errorf("create daemon TLS certificate: %w", err)
	}
	a.tls = daemonTLSCredential{
		Certificate: base64.RawStdEncoding.EncodeToString(der),
		PrivateKey:  base64.RawStdEncoding.EncodeToString(priv),
	}
	return atomicJSON(path, a.tls, 0o600)
}

func validateDaemonTLS(identity daemonTLSCredential) error {
	der, certErr := base64.RawStdEncoding.DecodeString(identity.Certificate)
	key, keyErr := base64.RawStdEncoding.DecodeString(identity.PrivateKey)
	cert, parseErr := x509.ParseCertificate(der)
	if certErr != nil || keyErr != nil || parseErr != nil || len(key) != ed25519.PrivateKeySize ||
		cert.VerifyHostname(daemonTLSServerName) != nil {
		return fmt.Errorf("invalid daemon TLS identity")
	}
	priv := ed25519.PrivateKey(key)
	if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		return fmt.Errorf("daemon TLS key does not match certificate")
	}
	return nil
}

func (a *FileAuthority) Revoke(_ context.Context, kind SubjectKind, subjectID string) error {
	if err := validateSubject(kind, subjectID); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return ErrAuthorityClosed
	}
	return a.revokeLocked(kind, subjectID, "")
}

// RevokeCurrent revokes only the exact principal generation supplied by its
// caller. Delayed completion cleanup uses this after proving that it still owns
// the corresponding runtime incarnation; it can never revoke a later regrant.
func (a *FileAuthority) RevokeCurrent(_ context.Context, principal Principal) error {
	if err := validateSubject(principal.Kind, principal.SubjectID); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return ErrAuthorityClosed
	}
	keyID := a.registry.Current[currentKey(principal.Kind, principal.SubjectID)]
	if keyID == "" || keyID != principal.KeyID {
		return ErrGenerationChanged
	}
	rec, ok := a.registry.Records[keyID]
	if !ok || rec.Generation != principal.Generation || rec.Kind != principal.Kind || rec.SubjectID != principal.SubjectID {
		return ErrGenerationChanged
	}
	return a.revokeLocked(principal.Kind, principal.SubjectID, keyID)
}

func (a *FileAuthority) revokeLocked(kind SubjectKind, subjectID, expectedKeyID string) error {
	key := currentKey(kind, subjectID)
	keyID := a.registry.Current[key]
	if keyID == "" {
		if expectedKeyID != "" {
			return ErrGenerationChanged
		}
		return nil
	}
	if expectedKeyID != "" && keyID != expectedKeyID {
		return ErrGenerationChanged
	}
	next := cloneRegistry(a.registry)
	rec, ok := next.Records[keyID]
	if !ok || rec.Kind != kind || rec.SubjectID != subjectID {
		return ErrInvalidCredential
	}
	if rec.RevokedAt == 0 {
		rec.RevokedAt = a.now().UnixMilli()
		next.Records[keyID] = rec
	}
	delete(next.Current, key)
	committed, err := a.commit(filepath.Join(a.root, "registry.json"), next, 0o600)
	if committed {
		a.registry = next
		a.invalidateLocked(keyID)
	}
	return err
}

// Verify authenticates and durably consumes a signed mailbox request. A
// duplicate is always ErrReplay; successful results are never replayed from this
// API, and callers must not blindly retry non-idempotent actions after a crash.
func (a *FileAuthority) Verify(_ context.Context, req SignedRequest) (Principal, error) {
	now := a.now()
	if req.Protocol != ProtocolVersion || subtle.ConstantTimeCompare([]byte(req.BootID), []byte(a.bootID)) != 1 ||
		!validHexID(req.RequestID, 16) || len(req.Body) == 0 || len(req.Body) > MaxBodyBytes {
		return Principal{}, ErrInvalidCredential
	}
	issued, expires := time.UnixMilli(req.IssuedAt), time.UnixMilli(req.ExpiresAt)
	if expires.Before(now.Add(-clockSkew)) || issued.After(now.Add(clockSkew)) ||
		expires.Before(issued) || expires.Sub(issued) > MaxRequestAge {
		return Principal{}, ErrExpired
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return Principal{}, ErrAuthorityClosed
	}
	rec, ok := a.registry.Records[req.KeyID]
	if !ok || rec.Generation != req.Generation || a.registry.Current[currentKey(rec.Kind, rec.SubjectID)] != req.KeyID {
		return Principal{}, ErrInvalidCredential
	}
	if rec.RevokedAt != 0 {
		return Principal{}, ErrRevoked
	}
	if rec.NotAfter <= now.UnixMilli() {
		return Principal{}, ErrInvalidCredential
	}
	pub, err := base64.RawStdEncoding.DecodeString(rec.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Principal{}, ErrInvalidCredential
	}
	sig, err := base64.RawStdEncoding.DecodeString(req.Signature)
	if err != nil || !ed25519.Verify(pub, signingBytes(req), sig) {
		return Principal{}, ErrInvalidCredential
	}
	replayKey := req.KeyID + ":" + fmt.Sprint(req.Generation) + ":" + req.RequestID
	if _, exists := a.replay.Entries[replayKey]; exists {
		return Principal{}, ErrReplay
	}
	nextReplay := cloneReplay(a.replay)
	for key, entry := range nextReplay.Entries {
		if entry.ExpiresAt < now.Add(-clockSkew).UnixMilli() {
			delete(nextReplay.Entries, key)
		}
	}
	if len(nextReplay.Entries) >= maxReplayEntries {
		return Principal{}, ErrCapacity
	}
	bodyHash := sha256.Sum256(req.Body)
	nextReplay.Entries[replayKey] = replayRecord{
		KeyID: req.KeyID, Generation: req.Generation, RequestID: req.RequestID,
		BodyDigest: hex.EncodeToString(bodyHash[:]), ExpiresAt: req.ExpiresAt,
	}
	if err := stateFits(nextReplay); err != nil {
		return Principal{}, err
	}
	committed, err := a.commit(filepath.Join(a.root, "replay.json"), nextReplay, 0o600)
	if committed {
		a.replay = nextReplay
	}
	if err != nil {
		return Principal{}, fmt.Errorf("persist replay acceptance: %w", err)
	}
	return Principal{KeyID: rec.KeyID, SubjectID: rec.SubjectID, Kind: rec.Kind, Generation: rec.Generation}, nil
}

func cloneReplay(in replayFile) replayFile {
	out := replayFile{Version: in.Version, Entries: make(map[string]replayRecord, len(in.Entries))}
	for key, value := range in.Entries {
		out.Entries[key] = value
	}
	return out
}

// Valid rechecks generation and revocation for long-lived connections before
// each action or pushed frame.
func (a *FileAuthority) Valid(_ context.Context, p Principal) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.rootDir == nil {
		return ErrAuthorityClosed
	}
	return a.validLocked(p)
}

func (a *FileAuthority) validLocked(p Principal) error {
	rec, ok := a.registry.Records[p.KeyID]
	if !ok || rec.SubjectID != p.SubjectID || rec.Kind != p.Kind || rec.Generation != p.Generation ||
		a.registry.Current[currentKey(rec.Kind, rec.SubjectID)] != p.KeyID {
		return ErrInvalidCredential
	}
	if rec.RevokedAt != 0 {
		return ErrRevoked
	}
	if rec.NotAfter <= a.now().UnixMilli() {
		return ErrInvalidCredential
	}
	return nil
}

// WatchInvalidation registers a long-lived connection for synchronous closure
// after a committed rotation or revocation. Registration and validity testing
// share the authority lock, so no invalidation can be missed between them.
func (a *FileAuthority) WatchInvalidation(p Principal) (<-chan struct{}, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rootDir == nil {
		return nil, nil, ErrAuthorityClosed
	}
	if err := a.validLocked(p); err != nil {
		return nil, nil, err
	}
	ch := make(chan struct{})
	if a.watchers[p.KeyID] == nil {
		a.watchers[p.KeyID] = make(map[chan struct{}]struct{})
	}
	a.watchers[p.KeyID][ch] = struct{}{}
	cancel := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if watchers := a.watchers[p.KeyID]; watchers != nil {
			delete(watchers, ch)
			if len(watchers) == 0 {
				delete(a.watchers, p.KeyID)
			}
		}
	}
	return ch, cancel, nil
}

func (a *FileAuthority) invalidateLocked(keyID string) {
	for ch := range a.watchers[keyID] {
		close(ch)
	}
	delete(a.watchers, keyID)
}

func LoadCredential(dir string) (Credential, error) {
	var cred Credential
	if err := readJSON(filepath.Join(dir, "current"), &cred); err != nil {
		return Credential{}, err
	}
	if cred.Protocol != ProtocolVersion {
		return Credential{}, ErrInvalidCredential
	}
	return cred, nil
}

func SignRequest(cred Credential, bootID string, body []byte, now time.Time, random io.Reader) (SignedRequest, error) {
	if len(body) == 0 || len(body) > MaxBodyBytes || !validHexID(bootID, 32) {
		return SignedRequest{}, ErrInvalidCredential
	}
	priv, err := base64.RawStdEncoding.DecodeString(cred.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return SignedRequest{}, ErrInvalidCredential
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return SignedRequest{}, err
	}
	req := SignedRequest{
		Protocol: ProtocolVersion, BootID: bootID, KeyID: cred.KeyID, Generation: cred.Generation,
		RequestID: hex.EncodeToString(nonce), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(MaxRequestAge).UnixMilli(),
		Body: append([]byte(nil), body...),
	}
	req.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, signingBytes(req)))
	return req, nil
}

func signingBytes(req SignedRequest) []byte {
	h := sha256.Sum256(req.Body)
	parts := [][]byte{
		[]byte("amux-access-request-v1"), []byte(req.BootID), []byte(req.KeyID),
		[]byte(fmt.Sprint(req.Generation)), []byte(req.RequestID), []byte(fmt.Sprint(req.IssuedAt)),
		[]byte(fmt.Sprint(req.ExpiresAt)), h[:],
	}
	var out []byte
	for _, part := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		out = append(out, n[:]...)
		out = append(out, part...)
	}
	return out
}

func validateSubject(kind SubjectKind, id string) error {
	if kind != SubjectHost && kind != SubjectProvider && kind != SubjectSession {
		return fmt.Errorf("unknown subject kind %q", kind)
	}
	if strings.TrimSpace(id) == "" || strings.ContainsRune(id, 0) {
		return fmt.Errorf("subject id is empty or invalid")
	}
	return nil
}

func validHexID(s string, bytes int) bool {
	if len(s) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func readJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return err
	}
	if len(buf) > maxStateBytes {
		return ErrCapacity
	}
	return json.Unmarshal(buf, out)
}

func stateFits(value any) error {
	buf, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(buf)+1 > maxStateBytes {
		return ErrCapacity
	}
	return nil
}

func writeJSONFile(path string, value any, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	encErr := json.NewEncoder(f).Encode(value)
	syncErr := f.Sync()
	closeErr := f.Close()
	if encErr != nil {
		return encErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	_, err := atomicJSONCommit(path, value, mode)
	return err
}

// atomicJSONCommit reports committed as soon as rename makes value the
// authoritative pathname. A later directory-sync failure is still an error,
// but callers must adopt the restrictive new in-memory state because disk may
// already contain it.
func atomicJSONCommit(path string, value any, mode os.FileMode) (committed bool, err error) {
	if err := stateFits(value); err != nil {
		return false, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(dir, ".access-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := json.NewEncoder(tmp).Encode(value); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, err
	}
	return true, syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
