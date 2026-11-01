package direct

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"amux/internal/core"
)

// ErrStoreBusy means another process owns the direct store (normally a running
// `amux direct serve`); administer it through the admin channel instead.
var ErrStoreBusy = errors.New("direct store is owned by another process")

// DefaultRoot is the direct state directory (core.DirectStateDir): resolved
// from the login account, not HOME, inside the state directory agent sandboxes
// never mount, and created 0700.
func DefaultRoot() (string, error) { return core.DirectStateDir() }

// ErrNoStore means no direct store exists yet; offline administration never
// creates one.
var ErrNoStore = errors.New("no direct store exists (run `amux direct serve` first)")

// stateVersion is the grants.json schema. Version 2 added PKCE-bound cloud
// enrollment codes and the cloud issuance policy. A version-1 reader (amux
// 15fcf0e and earlier) refuses version 2, so a downgrade can never read an
// outstanding cloud code as an unbound ordinary enrollment code. Version 1 is
// migrated forward atomically on open (see migrateLocked); nothing else is
// accepted and nothing is ever reinitialized.
const (
	stateVersion   = 2
	stateVersionS1 = 1
	grantsFile     = "grants.json"
	identityName   = "identity.json"
	lockName       = "lock"
	// AdminSocketName is the admin channel socket inside the root.
	AdminSocketName = "admin.sock"
)

type idemRecord struct {
	BodyHash string `json:"bodyHash"`
	State    string `json:"state"` // admitted | done | ambiguous
	Boot     string `json:"boot"`  // process that admitted it
	OK       bool   `json:"ok,omitempty"`
	Code     string `json:"code,omitempty"`
	Error    string `json:"error,omitempty"`
	At       int64  `json:"at"`
}

const (
	idemAdmitted = "admitted"
	idemDone     = "done"
	idemUnknown  = "ambiguous" // outcome unknown; never pruned, never re-run
)

type grantRecord struct {
	ClientID           string                 `json:"clientId"`
	Label              string                 `json:"label"`
	ClientKind         string                 `json:"clientKind"`
	Perms              []string               `json:"perms"`
	Resource           ResourceScope          `json:"resource"`
	IdentityGeneration string                 `json:"identityGeneration"`
	CreatedAt          int64                  `json:"createdAt"`
	AbsoluteExpiry     int64                  `json:"absoluteExpiry"`
	IdleTTL            int64                  `json:"idleTtl"`
	LastRefreshAt      int64                  `json:"lastRefreshAt"`
	RefreshHash        string                 `json:"refreshHash,omitempty"`
	UsedRefresh        []string               `json:"usedRefresh,omitempty"`
	RevokedAt          int64                  `json:"revokedAt,omitempty"`
	RevokeReason       string                 `json:"revokeReason,omitempty"`
	Idem               map[string]*idemRecord `json:"idem,omitempty"`
	// Provenance of a cloud-assisted enrollment (v2). It is set from the
	// redeemed code, never from client input, and survives any client_label.
	Origin         string `json:"origin,omitempty"`
	CloudRequester string `json:"cloudRequester,omitempty"`
}

type codeRecord struct {
	Label      string        `json:"label"`
	Perms      []string      `json:"perms"`
	Resource   ResourceScope `json:"resource"`
	CreatedAt  int64         `json:"createdAt"`
	ExpiresAt  int64         `json:"expiresAt"`
	ConsumedAt int64         `json:"consumedAt,omitempty"`
	ClientID   string        `json:"clientId,omitempty"`

	// Cloud-issued codes (v2). Origin is OriginCloud; a manual code has none.
	// Challenge is the RFC 7636 S256 code_challenge the redeemer must satisfy.
	Origin     string `json:"origin,omitempty"`
	Challenge  string `json:"challenge,omitempty"`
	PolicyGen  uint64 `json:"policyGen,omitempty"`
	Requester  string `json:"requester,omitempty"`
	ProviderID string `json:"providerId,omitempty"`
	IssueKey   string `json:"issueKey,omitempty"` // key into storeState.CloudIssues
}

// cloudPolicy is the host operator's cloud issuance policy. Only host-proven
// admin requests change it; every change bumps Generation, and a cloud code is
// redeemable only while the policy is enabled at the generation it was issued
// under.
type cloudPolicy struct {
	Enabled        bool          `json:"enabled"`
	Generation     uint64        `json:"generation"`
	Perms          []string      `json:"perms,omitempty"`
	Resource       ResourceScope `json:"resource"`
	OwnerPin       string        `json:"ownerPin,omitempty"`
	ProviderPin    string        `json:"providerPin,omitempty"`
	MaxOutstanding int           `json:"maxOutstanding,omitempty"`
	MaxPerHour     int           `json:"maxPerHour,omitempty"`
	Issued         []int64       `json:"issued,omitempty"` // issuance times within the last hour
	ChangedAt      int64         `json:"changedAt,omitempty"`
	DisableReason  string        `json:"disableReason,omitempty"`
}

// cloudIssue is the idempotency record of one cloud issuance key. Code is the
// replay material: the issued code itself, kept only while the code is
// unconsumed and unexpired so a lost relay reply can be retried with the same
// key without minting a second code. It is useless without the client's PKCE
// verifier, is never logged, and is cleared at consumption or expiry; the
// record then stays as a tombstone so the same key is refused, not reminted.
type cloudIssue struct {
	ReqHash   string `json:"reqHash"`
	CodeHash  string `json:"codeHash"`
	Code      string `json:"code,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

type storeState struct {
	V                  int                     `json:"v"`
	IdentityGeneration string                  `json:"identityGeneration"`
	Codes              map[string]*codeRecord  `json:"codes"`  // key: sha256(code)
	Grants             map[string]*grantRecord `json:"grants"` // key: client id
	Cloud              *cloudPolicy            `json:"cloud,omitempty"`
	CloudIssues        map[string]*cloudIssue  `json:"cloudIssues,omitempty"` // key: sha256(issue key)
}

// store is the on-disk registry. Exactly one process holds its lock; that
// process is the only writer.
type store struct {
	root  string
	lock  *os.File
	write func(dir, name string, data []byte) error // writeAtomic; tests inject failures
}

// errNotCommitted means the old content is still in place: nothing changed on
// disk and the caller must roll memory back. errAmbiguousCommit means the new
// content was renamed into place but its directory entry may not be durable:
// the caller keeps the new state in memory, refuses to hand out anything that
// depends on it, and the authority stops issuing until a restart re-reads disk.
var (
	errNotCommitted    = errors.New("direct store: write not committed")
	errAmbiguousCommit = errors.New("direct store: commit durability unknown")
)

// openStore creates root (0700) if needed and takes its exclusive lock without
// blocking. ErrStoreBusy means another owner holds it.
func openStore(root string) (*store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(root, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrStoreBusy
		}
		return nil, err
	}
	return &store{root: root, lock: f, write: writeAtomic}, nil
}

func (s *store) close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

func (s *store) path(name string) string { return filepath.Join(s.root, name) }

func (s *store) loadIdentity() (*Identity, error) {
	data, err := os.ReadFile(s.path(identityName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return unmarshalIdentity(data)
}

func (s *store) saveIdentity(id *Identity) error {
	data, err := id.marshal()
	if err != nil {
		return err
	}
	return s.write(s.root, identityName, data)
}

// loadState reads the registry. It reports whether the file is version 1 and
// must be migrated before anything else is written. A missing file is a new,
// empty version-2 registry.
func (s *store) loadState() (*storeState, bool, error) {
	data, err := os.ReadFile(s.path(grantsFile))
	if errors.Is(err, os.ErrNotExist) {
		return &storeState{V: stateVersion, Codes: map[string]*codeRecord{}, Grants: map[string]*grantRecord{}, CloudIssues: map[string]*cloudIssue{}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var st storeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, false, fmt.Errorf("direct grants: %w", err)
	}
	migrate := false
	switch st.V {
	case stateVersion:
	case stateVersionS1:
		// A version-1 registry has no cloud data by construction: version-1
		// binaries never write it. Fields that would only exist in version 2 mean
		// the file is not what it claims; refuse rather than guess.
		if st.Cloud != nil || len(st.CloudIssues) != 0 {
			return nil, false, errors.New("direct grants: version 1 file carries version 2 cloud data (refusing to load)")
		}
		for _, c := range st.Codes {
			if c.Origin != "" || c.Challenge != "" {
				return nil, false, errors.New("direct grants: version 1 file carries version 2 cloud data (refusing to load)")
			}
		}
		migrate = true
	default:
		return nil, false, fmt.Errorf("direct grants: unsupported version %d (refusing to reinitialize)", st.V)
	}
	if st.CloudIssues == nil {
		st.CloudIssues = map[string]*cloudIssue{}
	}
	if st.Codes == nil {
		st.Codes = map[string]*codeRecord{}
	}
	if st.Grants == nil {
		st.Grants = map[string]*grantRecord{}
	}
	return &st, migrate, nil
}

func (s *store) saveState(st *storeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.write(s.root, grantsFile, data)
}

// writeAtomic durably replaces dir/name: write a private temp file, fsync it,
// rename it over the target and fsync the directory, so a crash leaves either
// the old or the new content and success means the new content is on disk.
// Failures before the rename wrap errNotCommitted; after it, errAmbiguousCommit.
func writeAtomic(dir, name string, data []byte) error {
	if err := writeTemp(dir, name, data); err != nil {
		return fmt.Errorf("%w: %w", errNotCommitted, err)
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		d.Close()
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errAmbiguousCommit, err)
	}
	return nil
}

func writeTemp(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}
