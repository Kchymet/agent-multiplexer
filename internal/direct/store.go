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

// DefaultRoot is the direct state directory. It lives under the amux state
// directory, which agent sandboxes never mount (see core.StateDir and the
// panespec manifests), and is created 0700.
func DefaultRoot() string { return core.DirectStateDir() }

const (
	stateVersion = 1
	grantsFile   = "grants.json"
	identityName = "identity.json"
	lockName     = "lock"
	// AdminSocketName is the admin channel socket inside the root.
	AdminSocketName = "admin.sock"
)

type idemRecord struct {
	BodyHash string `json:"bodyHash"`
	State    string `json:"state"` // admitted | done
	Boot     string `json:"boot"`  // process that admitted it
	OK       bool   `json:"ok,omitempty"`
	Code     string `json:"code,omitempty"`
	Error    string `json:"error,omitempty"`
	At       int64  `json:"at"`
}

const (
	idemAdmitted = "admitted"
	idemDone     = "done"
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
}

type codeRecord struct {
	Label      string        `json:"label"`
	Perms      []string      `json:"perms"`
	Resource   ResourceScope `json:"resource"`
	CreatedAt  int64         `json:"createdAt"`
	ExpiresAt  int64         `json:"expiresAt"`
	ConsumedAt int64         `json:"consumedAt,omitempty"`
	ClientID   string        `json:"clientId,omitempty"`
}

type storeState struct {
	V                  int                     `json:"v"`
	IdentityGeneration string                  `json:"identityGeneration"`
	Codes              map[string]*codeRecord  `json:"codes"`  // key: sha256(code)
	Grants             map[string]*grantRecord `json:"grants"` // key: client id
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

func (s *store) loadState() (*storeState, error) {
	data, err := os.ReadFile(s.path(grantsFile))
	if errors.Is(err, os.ErrNotExist) {
		return &storeState{V: stateVersion, Codes: map[string]*codeRecord{}, Grants: map[string]*grantRecord{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st storeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("direct grants: %w", err)
	}
	if st.V != stateVersion {
		return nil, fmt.Errorf("direct grants: unsupported version %d (refusing to reinitialize)", st.V)
	}
	if st.Codes == nil {
		st.Codes = map[string]*codeRecord{}
	}
	if st.Grants == nil {
		st.Grants = map[string]*grantRecord{}
	}
	return &st, nil
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
