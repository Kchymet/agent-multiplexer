// Package hostprep provides descriptor-anchored access to files that amux
// prepares inside a session before entering the session's namespace.
package hostprep

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafe marks an untrusted destination alias, escape, or identity change.
// Launch callers must refuse these errors rather than treating preparation as
// optional. Ordinary I/O failures retain the caller's existing best-effort
// policy where no isolation boundary depends on the output.
var ErrUnsafe = errors.New("unsafe session preparation path")

// MaxReadFileSize bounds files consumed wholly in memory during configuration
// preparation. Conversation transcripts and other intentionally large inputs
// use OpenFile or AtomicWriteFrom and stream instead.
const MaxReadFileSize = 16 << 20

// ErrFileTooLarge reports that a whole-file configuration read exceeded the
// explicit preparation limit.
var ErrFileTooLarge = errors.New("hostprep: configuration file exceeds 16 MiB")

// IsUnsafe reports whether err represents a path-boundary violation.
func IsUnsafe(err error) bool { return errors.Is(err, ErrUnsafe) }

func unsafe(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafe, fmt.Sprintf(format, args...))
}

// Root pins a session directory for one host-side preparation transaction.
// Mutable destination paths are resolved one component at a time without
// accepting symlinks. The embedded os.Root supplies descriptor-relative
// containment and keeps referring to the opened directory after a rename.
type Root struct {
	dir  *os.Root
	name string
}

// OpenSession opens name as an authoritative session root. The initial lstat
// rejects a static symlink; comparing it with the opened directory detects a
// replacement between check and open. Once returned, later parent swaps cannot
// redirect operations because os.Root pins the opened directory.
func OpenSession(name string) (*Root, error) {
	if name == "" {
		return nil, errors.New("hostprep: empty session root")
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, fmt.Errorf("hostprep: session root: %w", err)
	}
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("hostprep: lstat session root: %w", err)
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, unsafe("session root is not a real directory: %s", abs)
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("hostprep: open session root: %w", err)
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		_ = r.Close()
		if err != nil {
			return nil, fmt.Errorf("hostprep: stat opened session root: %w", err)
		}
		return nil, unsafe("session root changed while opening")
	}
	return &Root{dir: r, name: abs}, nil
}

func newRoot(r *os.Root, name string) *Root { return &Root{dir: r, name: name} }

// Close releases the pinned directory handle.
func (r *Root) Close() error { return r.dir.Close() }

// Name is the absolute path originally used to open the root. It is for
// messages and environment values only; security-sensitive I/O uses dir.
func (r *Root) Name() string { return r.name }

// Rel converts an absolute destination path beneath this root to a relative
// name. Relative inputs are cleaned and accepted directly. Lexical escapes are
// rejected before os.Root's stronger resolution checks.
func (r *Root) Rel(name string) (string, error) {
	if name == "" {
		return "", errors.New("hostprep: empty path")
	}
	rel := name
	var err error
	if filepath.IsAbs(name) {
		rel, err = filepath.Rel(r.name, filepath.Clean(name))
		if err != nil {
			return "", err
		}
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return rel, nil
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", unsafe("path escapes session root: %s", name)
	}
	return rel, nil
}

// Sub opens rel as a pinned, real directory. Every component is checked and
// opened separately, so neither an existing symlink nor a check/open swap is
// accepted. Missing components are created when create is true.
func (r *Root) Sub(rel string, create bool, perm fs.FileMode) (*Root, error) {
	rel, err := r.Rel(rel)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return nil, errors.New("hostprep: cannot duplicate root handle")
	}
	cur := r
	owned := false
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			if owned {
				_ = cur.Close()
			}
			return nil, unsafe("invalid path component %q", part)
		}
		next, err := cur.openChild(part, create, perm)
		if owned {
			_ = cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur, owned = next, true
		if i == len(parts)-1 {
			return cur, nil
		}
	}
	panic("unreachable")
}

func (r *Root) openChild(name string, create bool, perm fs.FileMode) (*Root, error) {
	for attempts := 0; attempts < 2; attempts++ {
		before, err := r.dir.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) && create && attempts == 0 {
			if err := r.dir.Mkdir(name, perm.Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			return nil, unsafe("destination component %q is not a real directory", name)
		}
		next, err := r.dir.OpenRoot(name)
		if err != nil {
			return nil, unsafe("destination component %q changed while opening: %v", name, err)
		}
		after, err := next.Stat(".")
		if err != nil || !os.SameFile(before, after) {
			_ = next.Close()
			if err != nil {
				return nil, unsafe("could not validate destination component %q: %v", name, err)
			}
			return nil, unsafe("destination component %q changed while opening", name)
		}
		return newRoot(next, filepath.Join(r.name, name)), nil
	}
	return nil, fs.ErrNotExist
}

func split(rel string) (dir, base string, err error) {
	rel = filepath.Clean(rel)
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", unsafe("invalid file path %q", rel)
	}
	return filepath.Dir(rel), filepath.Base(rel), nil
}

func (r *Root) parent(rel string, create bool, perm fs.FileMode) (*Root, string, func(), error) {
	rel, err := r.Rel(rel)
	if err != nil {
		return nil, "", nil, err
	}
	dir, base, err := split(rel)
	if err != nil {
		return nil, "", nil, err
	}
	if dir == "." {
		return r, base, func() {}, nil
	}
	p, err := r.Sub(dir, create, perm)
	if err != nil {
		return nil, "", nil, err
	}
	return p, base, func() { _ = p.Close() }, nil
}

// ReadFile reads an existing, unlinked regular destination file without
// following its final symlink. A multiply linked file is rejected because its
// contents may belong to another same-filesystem path outside this session.
func (r *Root) ReadFile(rel string) ([]byte, error) {
	f, err := r.OpenFile(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxReadFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxReadFileSize {
		return nil, ErrFileTooLarge
	}
	return b, nil
}

// OpenFile securely opens an existing regular file for reading.
func (r *Root) OpenFile(rel string) (*os.File, error) {
	p, base, done, err := r.parent(rel, false, 0)
	if err != nil {
		return nil, err
	}
	defer done()
	before, err := p.dir.Lstat(base)
	if err != nil {
		return nil, err
	}
	if err := safeRegular(before); err != nil {
		return nil, fmt.Errorf("hostprep: %s: %w", rel, err)
	}
	f, err := openReadFile(p.dir, base)
	if err != nil {
		return nil, unsafe("%s changed while opening: %v", rel, err)
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = f.Close()
		if err != nil {
			return nil, unsafe("could not validate opened %s: %v", rel, err)
		}
		return nil, unsafe("%s changed while opening", rel)
	}
	if err := safeRegular(after); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("hostprep: %s: %w", rel, err)
	}
	return f, nil
}

// StatFile returns verified metadata for an existing unlinked regular file.
func (r *Root) StatFile(rel string) (fs.FileInfo, error) {
	f, err := r.OpenFile(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func safeRegular(fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
		return unsafe("destination is not a regular file")
	}
	if n := linkCount(fi); n > 1 {
		return unsafe("destination has %d hard links", n)
	}
	return nil
}

// Lstat returns metadata for a path without following its final symlink. Its
// parent components are descriptor-anchored and may not be symlinks.
func (r *Root) Lstat(rel string) (fs.FileInfo, error) {
	p, base, done, err := r.parent(rel, false, 0)
	if err != nil {
		return nil, err
	}
	defer done()
	return p.dir.Lstat(base)
}

// ReadDir reads a real directory without accepting symlink components.
func (r *Root) ReadDir(rel string) ([]fs.DirEntry, error) {
	rel, err := r.Rel(rel)
	if err != nil {
		return nil, err
	}
	if rel == "." {
		return fs.ReadDir(r.dir.FS(), ".")
	}
	sub, err := r.Sub(rel, false, 0)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	return fs.ReadDir(sub.dir.FS(), ".")
}

// WalkRegular walks the regular files below rel without following symlinks or
// special files. Each callback receives a slash-separated name relative to rel,
// verified metadata, and securely read bytes. Concurrent replacements fail or
// are skipped by the caller rather than escaping the root.
func (r *Root) WalkRegular(rel string, fn func(name string, info fs.FileInfo, data []byte) error) error {
	return r.WalkFiles(rel, func(name string, info fs.FileInfo) error {
		data, err := r.readWalkFile(rel, name)
		if err != nil {
			return err
		}
		return fn(name, info, data)
	})
}

func (r *Root) readWalkFile(base, name string) ([]byte, error) {
	if base == "." {
		return r.ReadFile(name)
	}
	return r.ReadFile(filepath.Join(base, name))
}

// WalkFiles walks verified, unlinked regular files without reading their
// contents. Symlinks and special files are skipped.
func (r *Root) WalkFiles(rel string, fn func(name string, info fs.FileInfo) error) error {
	rel, err := r.Rel(rel)
	if err != nil {
		return err
	}
	base := r
	done := func() {}
	if rel != "." {
		base, err = r.Sub(rel, false, 0)
		if err != nil {
			return err
		}
		done = func() { _ = base.Close() }
	}
	defer done()
	err = base.walkFiles("", fn)
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func (r *Root) walkFiles(prefix string, fn func(name string, info fs.FileInfo) error) error {
	entries, err := fs.ReadDir(r.dir.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := name
		if prefix != "" {
			path = filepath.Join(prefix, name)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if entry.IsDir() {
			child, err := r.openChild(name, false, 0)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			err = child.walkFiles(path, fn)
			_ = child.Close()
			if err != nil {
				return err
			}
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		f, err := r.OpenFile(name)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil {
			err = f.Close()
		} else {
			_ = f.Close()
		}
		if err != nil {
			return err
		}
		if err := fn(path, info); err != nil {
			return err
		}
	}
	return nil
}

// Remove removes the named directory entry without following its final
// symlink. Parent symlinks are not accepted.
func (r *Root) Remove(rel string) error {
	p, base, done, err := r.parent(rel, false, 0)
	if err != nil {
		return err
	}
	defer done()
	return p.dir.Remove(base)
}

// AtomicWrite replaces rel with data using an unpredictable exclusive temp
// file and a descriptor-anchored rename. It never truncates rel or any planted
// predictable temp alias.
func (r *Root) AtomicWrite(rel string, data []byte, mode fs.FileMode) error {
	return r.AtomicWriteFrom(rel, bytes.NewReader(data), mode)
}

// AtomicWriteFrom is AtomicWrite for an intentionally large or streaming
// source. Publication is still an exclusive random temporary file followed by
// an anchored rename.
func (r *Root) AtomicWriteFrom(rel string, src io.Reader, mode fs.FileMode) error {
	p, base, done, err := r.parent(rel, true, 0o755)
	if err != nil {
		return err
	}
	defer done()
	var tmp string
	var f *os.File
	for attempts := 0; attempts < 32; attempts++ {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		tmp = ".amux-" + hex.EncodeToString(nonce[:]) + ".tmp"
		f, err = p.dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	if f == nil {
		return errors.New("hostprep: could not allocate temporary file")
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = p.dir.Remove(tmp)
		}
	}()
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return unsafe("could not validate temporary publication file: %v", err)
	}
	named, err := p.dir.Lstat(tmp)
	if err != nil || !os.SameFile(opened, named) {
		_ = f.Close()
		if err != nil {
			return unsafe("temporary publication file changed: %v", err)
		}
		return unsafe("temporary publication file changed before rename")
	}
	if err := safeRegular(named); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	named, err = p.dir.Lstat(tmp)
	if err != nil || !os.SameFile(opened, named) {
		if err != nil {
			return unsafe("temporary publication file changed after close: %v", err)
		}
		return unsafe("temporary publication file changed before rename")
	}
	if err := safeRegular(named); err != nil {
		return err
	}
	if err := p.dir.Rename(tmp, base); err != nil {
		return err
	}
	removeTemp = false
	published, err := p.dir.Lstat(base)
	if err != nil || !os.SameFile(opened, published) {
		if err != nil {
			return unsafe("published destination changed: %v", err)
		}
		return unsafe("published destination changed after rename")
	}
	if err := safeRegular(published); err != nil {
		return err
	}
	return nil
}

// ReplaceSymlink installs the explicitly granted target at rel. Generic
// destination operations never follow this link; only callers that distinguish
// a trusted template source from an untrusted destination should use it.
func (r *Root) ReplaceSymlink(rel, target string) error {
	p, base, done, err := r.parent(rel, true, 0o755)
	if err != nil {
		return err
	}
	defer done()
	if err := p.dir.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return p.dir.Symlink(target, base)
}

// ReplaceHardlink installs an explicitly granted regular source file at rel.
// The destination directory is pinned; the source remains an exact trusted host
// path chosen by the caller. This is reserved for harnesses that refuse symlink
// credentials and must not be used for generic destination publication.
func (r *Root) ReplaceHardlink(rel, source string) error {
	p, base, done, err := r.parent(rel, true, 0o755)
	if err != nil {
		return err
	}
	defer done()
	if err := p.dir.Remove(base); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d, err := p.dir.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return linkExternal(source, d, base)
}
