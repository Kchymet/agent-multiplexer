package centralauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"amux/internal/core"
)

const (
	recordVersion  = 1
	credentialFile = "credential.json"
	writeLockFile  = ".write.lock"
	loginLockFile  = ".login.lock"
	// maxRecordBytes bounds what Load reads; a record is the token plus metadata.
	maxRecordBytes = 2 * maxSecretBytes
)

// Store is one harness's central credential directory. It lives beneath
// core.AuthDir(), which no pane mounts; only the daemon and host CLI read it.
type Store struct {
	Dir     string
	Harness Harness
	Kind    Kind
}

// ClaudeStore is the central Claude setup-token store. It is deliberately not
// the legacy shared store (core.AuthDir()/claude), which held a refresh token
// that every session could read and write.
func ClaudeStore() Store {
	return Store{Dir: filepath.Join(core.AuthDir(), "central", string(Claude)), Harness: Claude, Kind: KindClaudeOAuthToken}
}

// record is the on-disk form. The token and its metadata share one file so a
// replacement is a single atomic rename; a reader never sees a torn pair.
type record struct {
	Version     int       `json:"version"`
	Harness     Harness   `json:"harness"`
	Kind        Kind      `json:"kind"`
	Token       string    `json:"token"`
	Fingerprint string    `json:"fingerprint"`
	StoredAt    time.Time `json:"stored_at"`
	Generation  uint64    `json:"generation"`
}

// Status describes a store without exposing its value.
type Status struct {
	Configured  bool
	Fingerprint string
	StoredAt    time.Time
	Generation  uint64
	Path        string
}

func (s Store) path() string { return filepath.Join(s.Dir, credentialFile) }

// ensureDir creates the store privately and refuses one another user, a
// symlink, or a group/world-accessible mode could expose.
func (s Store) ensureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return checkPrivate(s.Dir, true)
}

func checkPrivate(path string, dir bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || fi.IsDir() != dir || (!dir && !fi.Mode().IsRegular()) {
		return fmt.Errorf("%w: %s has an unexpected file type", ErrInsecureStore, path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is accessible to other users (mode %v)", ErrInsecureStore, path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is owned by another user", ErrInsecureStore, path)
	}
	return nil
}

// flock opens and locks a lock file in the store. Nonblocking locks fail fast
// so a second interactive login reports the first instead of waiting on it.
func (s Store) flock(name string, nonblocking bool) (release func(), err error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, name), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if nonblocking {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// LockLogin serializes interactive host logins for this store. The OS releases
// the lock even if the login terminal or process dies.
func (s Store) LockLogin() (release func(), err error) {
	release, err = s.flock(loginLockFile, true)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("another amux %s login is running", s.Harness)
	}
	return release, err
}

// Save atomically replaces the stored credential and advances its generation.
// The previous value is overwritten only by the rename of a complete record.
func (s Store) Save(token Secret, now time.Time) (Credential, error) {
	return s.SaveKind(token, s.Kind, now)
}

// SaveKind replaces the single credential record, including its explicit mode.
func (s Store) SaveKind(token Secret, kind Kind, now time.Time) (Credential, error) {
	if kind != s.Kind && !(s.Harness == Claude && kind == KindClaudeAPIKey) {
		return Credential{}, ErrInvalidSecret
	}
	if token.Empty() {
		return Credential{}, ErrInvalidSecret
	}
	release, err := s.flock(writeLockFile, false)
	if err != nil {
		return Credential{}, err
	}
	defer release()
	var generation uint64 = 1
	if prior, err := s.Load(); err == nil {
		generation = prior.Generation + 1
	} else if !errors.Is(err, ErrNotConfigured) {
		// A corrupt record must not block a fresh login, but generations still
		// never repeat: fall back to the wall clock, which exceeds any counter.
		generation = uint64(now.UnixNano())
	}
	marker := filepath.Join(s.Dir, "generation")
	if e := checkPrivate(marker, false); e == nil {
		b, e := os.ReadFile(marker)
		if e != nil {
			return Credential{}, e
		}
		n, e := strconv.ParseUint(string(b), 10, 64)
		if e != nil || n == ^uint64(0) {
			return Credential{}, errors.New("invalid credential generation")
		}
		if n >= generation {
			generation = n + 1
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return Credential{}, e
	}
	// Commit the counter first: crashes may skip a generation, never reuse one.
	if err := writePrivateAtomic(s.Dir, "generation", []byte(strconv.FormatUint(generation, 10))); err != nil {
		return Credential{}, err
	}
	rec := record{
		Version: recordVersion, Harness: s.Harness, Kind: kind,
		Token: token.Reveal(), Fingerprint: token.Fingerprint(),
		StoredAt: now.UTC().Truncate(time.Second), Generation: generation,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return Credential{}, err
	}
	if err := writePrivateAtomic(s.Dir, credentialFile, b); err != nil {
		return Credential{}, err
	}
	return rec.credential(token), nil
}

func writePrivateAtomic(dir, name string, b []byte) error {
	f, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Load reads the stored credential, verifying the store is private first.
func (s Store) Load() (Credential, error) {
	rec, err := s.read()
	if err != nil {
		return Credential{}, err
	}
	token, err := NewSecret(rec.Token)
	if err != nil {
		return Credential{}, fmt.Errorf("%s: stored %w", s.path(), err)
	}
	return rec.credential(token), nil
}

func (s Store) read() (record, error) {
	if err := checkPrivate(s.Dir, true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return record{}, ErrNotConfigured
		}
		return record{}, err
	}
	if err := checkPrivate(s.path(), false); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return record{}, ErrNotConfigured
		}
		return record{}, err
	}
	f, err := os.OpenFile(s.path(), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return record{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return record{}, err
	}
	if len(b) > maxRecordBytes {
		return record{}, fmt.Errorf("%s: credential record too large", s.path())
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		// Never echo the record: a decode error can quote its contents.
		return record{}, fmt.Errorf("%s: unreadable credential record", s.path())
	}
	if rec.Version != recordVersion || rec.Harness != s.Harness || !(rec.Kind == s.Kind || (s.Harness == Claude && (rec.Kind == KindClaudeOAuthToken || rec.Kind == KindClaudeAPIKey))) {
		return record{}, fmt.Errorf("%s: unsupported credential record (version %d, %s/%s)", s.path(), rec.Version, rec.Harness, rec.Kind)
	}
	return rec, nil
}

func (r record) credential(token Secret) Credential {
	return Credential{Harness: r.Harness, Kind: r.Kind, Token: token, StoredAt: r.StoredAt, Generation: r.Generation}
}

// Status reports whether a usable credential is stored, without its value.
// A store that exists but cannot be used returns its error alongside.
func (s Store) Status() (Status, error) {
	st := Status{Path: s.path()}
	c, err := s.Load()
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return st, nil
		}
		return st, err
	}
	st.Configured, st.Fingerprint, st.StoredAt, st.Generation = true, c.Token.Fingerprint(), c.StoredAt, c.Generation
	return st, nil
}

// Delete removes amux's setup-token only. Tokens already delivered to a running
// process cannot be retracted; account delivery fencing precedes this cleanup.
func (s Store) Delete() error {
	release, err := s.flock(writeLockFile, false)
	if err != nil {
		return err
	}
	defer release()
	if cred, e := s.Load(); e == nil {
		if e = writePrivateAtomic(s.Dir, "generation", []byte(strconv.FormatUint(cred.Generation, 10))); e != nil {
			return e
		}
	}
	err = os.Remove(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
